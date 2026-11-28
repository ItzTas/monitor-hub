package pages

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

const (
	msgChooseOption = "Choose one of the options."
	msgNoName       = "This new variable has a value but no name."
	msgRepeatedName = "Two new rows use this name."
)

// changeFrom is what the sent form changes against a fresh View. A value goes in only when it
// differs by value from the View's, so a value from the .env drawn and sent back untouched
// is never saved. The error is a revision that is not a number.
func changeFrom(view monitorconfig.View, sub submission) (monitorconfig.Change, []monitorconfig.Problem, error) {
	revision, err := strconv.ParseInt(sub.Revision, 10, 64)
	if err != nil {
		return monitorconfig.Change{}, nil, fmt.Errorf("revision %q: %w", sub.Revision, err)
	}
	c := changer{view: view, sub: sub, change: monitorconfig.Change{Revision: &revision}, problems: slices.Clone(sub.Problems)}
	if view.Declaration != nil {
		c.document()
		c.secrets()
		c.variables()
		c.newRows()
	}
	for _, v := range view.Ignored {
		if sub.Drop[v.Path] {
			c.change.DeleteIgnored = append(c.change.DeleteIgnored, v.Path)
		}
	}
	return c.change, c.problems, nil
}

type changer struct {
	view     monitorconfig.View
	sub      submission
	change   monitorconfig.Change
	problems []monitorconfig.Problem
}

func (c *changer) document() {
	long := tooManyItems(c.view.Document)
	for _, f := range c.view.Declaration.Fields {
		if !f.Editable || f.Secret || long && holdsList(f) {
			continue
		}
		state := c.fieldState(f.Name)
		if c.sub.Unset[f.Name] && state.Saved {
			put(&c.change.Document, f.Name, json.RawMessage("null"))
			continue
		}
		if !c.choicesFit(f, f.Name, true) {
			continue
		}
		value, ok := c.sub.Values[f.Name]
		if !ok {
			// A select or a number box left empty at the top means no value from the page.
			if text, sent := c.sub.Texts[f.Name]; sent && text == "" && state.Saved {
				put(&c.change.Document, f.Name, json.RawMessage("null"))
			}
			continue
		}
		if reflect.DeepEqual(normalized(value, true), normalized(c.view.Document[f.Name], true)) {
			continue
		}
		data, err := json.Marshal(value)
		if err != nil {
			continue
		}
		put(&c.change.Document, f.Name, json.RawMessage(data))
	}
}

func (c *changer) fieldState(name string) monitorconfig.FieldState {
	i := slices.IndexFunc(c.view.Fields, func(s monitorconfig.FieldState) bool { return s.Name == name })
	if i < 0 {
		return monitorconfig.FieldState{}
	}
	return c.view.Fields[i]
}

// choicesFit records a problem for each choice at or under path whose text is not one of its
// options; at the top, the value in force is accepted too, since the .env may hold another.
func (c *changer) choicesFit(f settings.Field, path string, top bool) bool {
	switch f.Type {
	case settings.Object:
		fits := true
		for _, child := range f.Fields {
			fits = c.choicesFit(child, path+"."+child.Name, false) && fits
		}
		return fits
	case settings.List:
		fits := true
		item := settings.Field{Type: f.Items.Type, Options: f.Items.Options, Fields: f.Items.Fields}
		for i := range c.sub.Counts[path] {
			fits = c.choicesFit(item, fmt.Sprintf("%s[%d]", path, i), false) && fits
		}
		return fits
	case settings.Choice:
		text := c.sub.Texts[path]
		inForce, _ := c.view.Document[path].(string)
		if text == "" || slices.Contains(f.Options, text) || top && text == inForce {
			return true
		}
		c.problems = append(c.problems, monitorconfig.Problem{Path: path, Message: msgChooseOption})
		return false
	}
	return true
}

// secrets takes what was typed for an editable secret path; a clear box wins over the text.
func (c *changer) secrets() {
	editable := map[string]bool{}
	for _, f := range c.view.Declaration.Fields {
		editable[f.Name] = f.Editable
	}
	for _, s := range c.view.Secrets {
		top, _, _ := strings.Cut(s.Path, ".")
		if !editable[top] {
			continue
		}
		if c.sub.Clear[s.Path] && s.Saved {
			put(&c.change.Secrets, s.Path, nil)
			continue
		}
		if text, ok := c.sub.Secrets[s.Path]; ok {
			put(&c.change.Secrets, s.Path, &text)
		}
	}
}

// variables saves as a secret what the page does not show: a secret variable, or one only in
// the .env, whose value the hub cannot tell is not a secret.
func (c *changer) variables() {
	vars := &c.change.Variables
	for _, v := range c.view.Variables {
		if c.sub.VarUnset[v.Name] && v.Saved {
			vars.Unset = append(vars.Unset, v.Name)
			continue
		}
		sent, ok := c.sub.Variables[v.Name]
		if v.Secret || v.Value == nil {
			if sent != "" {
				put(&vars.Set, v.Name, sent)
				vars.Secret = append(vars.Secret, v.Name)
			}
			continue
		}
		if ok && sent != *v.Value {
			put(&vars.Set, v.Name, sent)
		}
	}
}

// newRows leaves the hub's own rules on names to Save.
func (c *changer) newRows() {
	vars := &c.change.Variables
	uses, reported := map[string]int{}, map[string]bool{}
	for _, r := range c.sub.New {
		uses[r.Name]++
	}
	for _, r := range c.sub.New {
		switch {
		case r.Name == "" && r.Value == "":
		case r.Name == "":
			c.problems = append(c.problems, monitorconfig.Problem{Path: "", Message: msgNoName})
		case uses[r.Name] > 1:
			if !reported[r.Name] {
				c.problems = append(c.problems, monitorconfig.Problem{Path: r.Name, Message: msgRepeatedName})
				reported[r.Name] = true
			}
		default:
			put(&vars.Set, r.Name, r.Value)
			if r.Secret {
				vars.Secret = append(vars.Secret, r.Name)
			}
		}
	}
}

func put[V any](m *map[string]V, key string, value V) {
	if *m == nil {
		*m = map[string]V{}
	}
	(*m)[key] = value
}

// normalized is v as plain JSON values with the empty entries of objects left out, and, at the
// top, an empty value as nil: a field the document lacks is drawn empty and sent back so.
func normalized(v any, top bool) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var plain any
	if json.Unmarshal(data, &plain) != nil {
		return v
	}
	plain = pruned(plain)
	if top && empty(plain) {
		return nil
	}
	return plain
}

func pruned(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, child := range x {
			if child = pruned(child); !empty(child) {
				out[k] = child
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = pruned(child)
		}
		return out
	}
	return v
}

func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// tooManyItems is whether the lists of document hold more items in all than a form may send:
// the page then leaves them out of the form, so a save keeps them as they are.
func tooManyItems(document map[string]any) bool {
	return countItems(document) > maxItems
}

func countItems(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		for _, child := range x {
			n += countItems(child)
		}
	case []any:
		n += len(x)
		for _, child := range x {
			n += countItems(child)
		}
	}
	return n
}

func holdsList(f settings.Field) bool {
	return f.Type == settings.List || slices.ContainsFunc(f.Fields, holdsList)
}
