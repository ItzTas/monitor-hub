package pages

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

// maxItems bounds the items of a whole submission, nested lists included, whatever the n:
// keys say: the form draws every item it counts.
const maxItems = 1000

var emptyIsNone = map[settings.Type]bool{settings.Integer: true, settings.Number: true, settings.Boolean: true, settings.Choice: true}

// parseSubmission reads a sent form by walking the declaration, so only what the form draws
// for an editable, non-secret field becomes a value.
func parseSubmission(view monitorconfig.View, form url.Values) submission {
	sub := submission{
		Revision:    form.Get("revision"),
		Declaration: form.Get("declaration"),
		Values:      map[string]any{},
		Texts:       map[string]string{},
		Counts:      map[string]int{},
		Indexes:     map[string][]int{},
		Secrets:     map[string]string{},
		Unset:       set(form["unset"]),
		Clear:       set(form["clear"]),
		Drop:        set(form["drop"]),
		Variables:   map[string]string{},
		VarUnset:    set(form["var_unset"]),
		New:         newRows(form),
	}
	r := &reader{form: form, sub: &sub, budget: maxItems}
	if view.Declaration != nil {
		for _, f := range view.Declaration.Fields {
			if !f.Editable || f.Secret {
				continue
			}
			if value, ok := r.value(f, f.Name); ok {
				sub.Values[f.Name] = value
			}
		}
	}
	for key := range form {
		if path, ok := strings.CutPrefix(key, "s:"); ok && form.Get(key) != "" {
			sub.Secrets[path] = form.Get(key)
		}
		if name, ok := strings.CutPrefix(key, "var."); ok {
			sub.Variables[name] = form.Get(key)
		}
	}
	return sub
}

type reader struct {
	form   url.Values
	sub    *submission
	read   int // form keys read, to tell whether an object was drawn
	budget int // items left to read, spent in the order of the declaration
}

func (r *reader) value(f settings.Field, path string) (any, bool) {
	if f.Type == settings.Object {
		return r.object(f.Fields, path)
	}
	if f.Type == settings.List {
		return r.list(f.Items, path)
	}
	return r.simple(f.Type, path, false)
}

// simple converts the text at path. An empty text is no value in a list item, and in a
// select or a number box anywhere; text fields keep it.
func (r *reader) simple(t settings.Type, path string, item bool) (any, bool) {
	key := "v:" + path
	if !r.form.Has(key) {
		return nil, false
	}
	r.read++
	text := r.form.Get(key)
	r.sub.Texts[path] = text
	if text == "" && (item || emptyIsNone[t]) {
		return nil, false
	}
	value, problem := monitorconfig.ConvertText(t, text)
	if problem != "" {
		r.sub.Problems = append(r.sub.Problems, monitorconfig.Problem{Path: path, Message: problem})
		return nil, false
	}
	return value, true
}

func (r *reader) object(fields []settings.Field, path string) (any, bool) {
	before := r.read
	out := map[string]any{}
	for _, f := range fields {
		if f.Secret {
			continue
		}
		if value, ok := r.value(f, path+"."+f.Name); ok {
			out[f.Name] = value
		}
	}
	return out, r.read > before
}

// list keeps, for each item it reads, the item's index in the form, since problems come back
// by the index in the saved document.
func (r *reader) list(it *settings.Item, path string) (any, bool) {
	key := "n:" + path
	if !r.form.Has(key) {
		return nil, false
	}
	r.read++
	n, _ := strconv.Atoi(r.form.Get(key))
	n = min(max(n, 0), r.budget)
	r.budget -= n
	r.sub.Counts[path] = n
	items, indexes := []any{}, []int{}
	for i := range n {
		at := fmt.Sprintf("%s[%d]", path, i)
		if value, ok := r.item(it, at); ok {
			items, indexes = append(items, value), append(indexes, i)
		}
	}
	r.sub.Indexes[path] = indexes
	return items, true
}

// item reads one entry of a list: an object item always stays, a simple one only with text.
func (r *reader) item(it *settings.Item, path string) (any, bool) {
	if it.Type == settings.Object {
		value, _ := r.object(it.Fields, path)
		return value, true
	}
	return r.simple(it.Type, path, true)
}

// newRows pairs the new variables by position; a missing value or kind is empty.
func newRows(form url.Values) []newRow {
	names, values, kinds := form["new_name"], form["new_value"], form["new_kind"]
	rows := make([]newRow, max(len(names), len(values), len(kinds)))
	at := func(list []string, i int) string {
		if i < len(list) {
			return list[i]
		}
		return ""
	}
	for i := range rows {
		rows[i] = newRow{Name: strings.TrimSpace(at(names, i)), Value: at(values, i), Secret: at(kinds, i) == "secret"}
	}
	return rows
}

func set(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// listEdit is what an operation does to a list: grow changes its length, and move gives each
// item its new index, or false to drop it.
type listEdit struct {
	grow int
	move func(i int) (int, bool)
}

// applyOp edits a list of the sent form, keeping every value where it goes; an operation on
// anything but a drawn list or item, or an add past maxItems, gives sub back as it was.
// Only Texts and Counts move: Values, Indexes and Problems still describe the form before the op.
func applyOp(decl *settings.Declaration, sub submission, op string) submission {
	swap := func(a, b int) func(int) (int, bool) {
		return func(j int) (int, bool) {
			if j == a {
				return b, true
			}
			if j == b {
				return a, true
			}
			return j, true
		}
	}
	ops := map[string]func(i, n int) (listEdit, bool){
		// The total holds the list too, so a list at its own cap is refused as well.
		"add": func(_, _ int) (listEdit, bool) { return listEdit{grow: 1}, total(sub.Counts) < maxItems },
		"remove": func(i, n int) (listEdit, bool) {
			drop := func(j int) (int, bool) {
				if j > i {
					return j - 1, true
				}
				return j, j != i
			}
			return listEdit{grow: -1, move: drop}, i < n
		},
		"up":   func(i, n int) (listEdit, bool) { return listEdit{move: swap(i, i-1)}, 0 < i && i < n },
		"down": func(i, n int) (listEdit, bool) { return listEdit{move: swap(i, i+1)}, i < n-1 },
	}
	kind, target, _ := strings.Cut(op, ":")
	do, ok := ops[kind]
	if !ok {
		return sub
	}
	// i is an item's index for every operation but add.
	list, i := target, -1
	if kind != "add" {
		list, i = itemOf(target)
	}
	n, drawn := sub.Counts[list]
	if !drawn || !decl.Declares(list+"[0]") {
		return sub
	}
	edit, ok := do(i, n)
	if !ok {
		return sub
	}
	sub.Texts = moved(sub.Texts, list, edit.move)
	sub.Counts = moved(sub.Counts, list, edit.move)
	sub.Counts[list] = n + edit.grow
	return sub
}

func total(counts map[string]int) int {
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}

// itemOf splits rules[2] into rules and 2; i is -1 when path does not end in an index.
func itemOf(path string) (string, int) {
	open := strings.LastIndex(path, "[")
	if open < 0 || !strings.HasSuffix(path, "]") {
		return "", -1
	}
	i, err := strconv.Atoi(path[open+1 : len(path)-1])
	if err != nil || i < 0 {
		return "", -1
	}
	return path[:open], i
}

// moved copies m with the keys under the items of list moved as move says.
func moved[V any](m map[string]V, list string, move func(int) (int, bool)) map[string]V {
	out := make(map[string]V, len(m))
	for key, v := range m {
		i, rest, under := underItem(key, list)
		if !under || move == nil {
			out[key] = v
			continue
		}
		if j, keep := move(i); keep {
			out[fmt.Sprintf("%s[%d]%s", list, j, rest)] = v
		}
	}
	return out
}

// underItem splits rules[1].actions[0] under rules into 1 and .actions[0].
func underItem(key, list string) (int, string, bool) {
	rest, ok := strings.CutPrefix(key, list+"[")
	if !ok {
		return 0, "", false
	}
	digits, rest, ok := strings.Cut(rest, "]")
	if !ok {
		return 0, "", false
	}
	i, err := strconv.Atoi(digits)
	return i, rest, err == nil
}
