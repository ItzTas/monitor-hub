package pages

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

// submission is what a sent form holds, by path. Secrets and the typed values of secret
// variables are read only to tell that something secret was typed: they are never drawn.
// Indexes gives, for each list, the form index of each item in Values.
type submission struct {
	Revision, Declaration string
	Values                map[string]any
	Texts                 map[string]string
	Counts                map[string]int
	Indexes               map[string][]int
	Secrets               map[string]string
	Unset                 map[string]bool
	Clear                 map[string]bool
	Drop                  map[string]bool
	Variables             map[string]string
	VarUnset              map[string]bool
	New                   []newRow
	Problems              []monitorconfig.Problem
}

type newRow struct {
	Name, Value string
	Secret      bool
}

// editor is the configuration form of one monitor, as editor.html draws it.
type editor struct {
	Monitor     string
	Declared    bool
	Revision    int64
	Declaration string
	Outcome     outcome
	Buttons     buttons
	EnvProblems []monitorconfig.Problem
	Summary     []summaryLine
	TypedSecret bool
	Groups      []group
	Variables   []variableRow
	NewRows     []newRowView
	Ignored     []ignoredRow
}

// outcome is what the last save or restart came to. Focus is for the answer to a save, so the
// result takes the focus; what the poll brings never does.
type outcome struct {
	Monitor    string
	Restarting bool
	Waiting    bool // polls for an earlier save's restart without saying this one saved
	Message    string
	Focus      bool
	Settled    monitorconfig.ProgressState
	State      string
	StateText  string
	Said       string
	Reason     string
	Phase      string
}

// buttons are the visible save button and the hidden one Enter presses; OutOfBand swaps them
// in from a progress answer.
type buttons struct {
	Restarting bool
	OutOfBand  bool
}

// summaryLine links to where its problem is drawn, when it is.
type summaryLine struct {
	Target string
	Text   string
}

type group struct {
	Legend string
	Fields []field
}

type selectOption struct {
	Value, Text string
	Selected    bool
}

type checkbox struct {
	Name, Value, Text string
	Checked           bool
}

// field is one declared field or list item at its path; Kind picks how field.html draws it:
// text, select, password, object, list or readonly.
type field struct {
	Kind        string
	Path        string
	ID          string
	Label       string
	Required    bool
	Notes       []string
	Source      string
	Reason      string
	Error       string
	InputMode   string
	Placeholder string
	Value       string
	Options     []selectOption
	DescribedBy string
	Back        *checkbox
	Children    []field
	Items       []item
	Action      string
}

// item is one entry of a list: a simple Field, or the Children of an object.
type item struct {
	Path        string
	N           int
	First, Last bool
	Field       field
	Children    []field
}

type variableRow struct {
	Name        string
	ID          string
	Source      string
	Value       string
	Password    bool
	Error       string
	DescribedBy string
	Back        *checkbox
}

type newRowView struct {
	I      int
	Name   string
	Value  string
	Secret bool
}

type ignoredRow struct {
	I      int
	Path   string
	Reason string
	Value  string
	Secret bool
	Error  string
	Drop   bool
}

func buildEditor(name string, view monitorconfig.View, progress monitorconfig.Progress, posted *submission, problems []monitorconfig.Problem) editor {
	restarting := progress.State == monitorconfig.ProgressRestarting
	e := editor{
		Monitor:     name,
		Declared:    view.Declared,
		Revision:    view.Revision,
		Declaration: declarationKey(view),
		Outcome:     outcome{Monitor: name, Restarting: restarting},
		Buttons:     buttons{Restarting: restarting},
		EnvProblems: view.Problems,
	}
	b := newBuilder(name, view, posted, problems)
	b.long = tooManyItems(view.Document)
	if view.Declared {
		e.Groups = b.groups()
		e.Variables = b.variables()
		e.NewRows = b.newRows()
		e.TypedSecret = b.typedSecret()
	}
	e.Ignored = b.ignored()
	e.Summary = b.summary(problems)
	return e
}

type builder struct {
	view    monitorconfig.View
	posted  *submission
	errs    map[string]string
	drawn   map[string]string // problem path → id of where it is drawn
	action  string
	long    bool // the lists hold more items than a form may send
	fields  map[string]monitorconfig.FieldState
	secrets map[string]monitorconfig.SecretState
}

func newBuilder(name string, view monitorconfig.View, posted *submission, problems []monitorconfig.Problem) *builder {
	b := &builder{
		view:    view,
		posted:  posted,
		errs:    map[string]string{},
		drawn:   map[string]string{},
		action:  "/settings/" + name + "/form",
		fields:  map[string]monitorconfig.FieldState{},
		secrets: map[string]monitorconfig.SecretState{},
	}
	for _, p := range problems {
		if _, ok := b.errs[p.Path]; !ok {
			b.errs[p.Path] = p.Message
		}
	}
	for _, f := range view.Fields {
		b.fields[f.Name] = f
	}
	for _, s := range view.Secrets {
		b.secrets[s.Path] = s
	}
	return b
}

// groups keeps the order in which each group first appears; fields without one are General.
func (b *builder) groups() []group {
	var groups []group
	at := map[string]int{}
	for _, f := range b.view.Declaration.Fields {
		legend := cmp.Or(f.Group, "General")
		i, ok := at[legend]
		if !ok {
			i, at[legend] = len(groups), len(groups)
			groups = append(groups, group{Legend: legend})
		}
		state := b.fields[f.Name]
		groups[i].Fields = append(groups[i].Fields, b.field(f, f.Name, b.view.Document[f.Name], &state))
	}
	return groups
}

// field builds f at path from its value in the document; top is its state when f is a
// top-level field.
func (b *builder) field(f settings.Field, path string, value any, top *monitorconfig.FieldState) field {
	type control struct{ kind, inputMode, example string }
	controls := map[settings.Type]control{
		settings.String:   {kind: "text"},
		settings.Duration: {kind: "text", example: "A duration such as 15m"},
		settings.Integer:  {kind: "text", inputMode: "numeric", example: "A whole number"},
		settings.Number:   {kind: "text", inputMode: "decimal", example: "A number such as 2.5"},
		settings.Boolean:  {kind: "select"},
		settings.Choice:   {kind: "select"},
		settings.Object:   {kind: "object"},
		settings.List:     {kind: "list"},
	}
	c, ok := controls[f.Type]
	if !ok {
		c = controls[settings.String]
	}
	out := field{Kind: c.kind, Path: path, ID: idSuffix(path), Label: f.Label, Required: f.Required, Action: b.action}
	shown := defaultText(f)
	out.Notes = notes(f.Help, prefixed("Default when empty: ", shown))
	if top != nil {
		empty := value == ""
		if f.Secret {
			empty = !b.secrets[path].Set
		}
		out.Source = sourceText(f.Secret, top.Source, empty)
	}
	if !f.Editable {
		out.Kind, out.Reason = "readonly", f.Reason
		return out
	}
	if top != nil && b.long && holdsList(f) {
		out.Kind, out.Reason = "readonly", msgTooManyItems
		return out
	}
	if f.Secret {
		return b.secret(f, out, top)
	}
	if top != nil && top.Saved {
		out.Back = &checkbox{Name: "unset", Value: path, Text: backText(f, top.EnvSet), Checked: b.posted != nil && b.posted.Unset[path]}
	}
	text := b.text(path, value)
	switch c.kind {
	case "object":
		out.Children = b.children(f.Fields, path, value)
		return b.withError(out)
	case "list":
		out.Items = b.items(f, path, value)
		return b.withError(out)
	case "select":
		out.Options = selectOptions(f, text, shown)
	default:
		out.Value, out.InputMode, out.Placeholder = text, c.inputMode, cmp.Or(shown, c.example)
	}
	return b.withError(out)
}

// secret draws an always empty password box: the page never holds a secret.
func (b *builder) secret(f settings.Field, out field, top *monitorconfig.FieldState) field {
	state := b.secrets[out.Path]
	out.Kind = "password"
	out.Notes = append(out.Notes, "Leave it empty to keep the current value.")
	if top == nil {
		source := monitorconfig.SourceUnset
		if state.Saved {
			source = monitorconfig.SourcePage
		}
		out.Source = sourceText(true, source, !state.Set)
	}
	if state.Saved {
		out.Back = &checkbox{Name: "clear", Value: out.Path, Text: backText(f, state.EnvSet), Checked: b.posted != nil && b.posted.Clear[out.Path]}
	}
	return b.withError(out)
}

// withError puts the problem of a drawn control next to it, and describes the control.
func (b *builder) withError(out field) field {
	if message, ok := b.errs[out.Path]; ok {
		out.Error = message
		b.drawn[out.Path] = controlID(out.Path)
	}
	var ids []string
	for _, d := range []struct {
		prefix  string
		present bool
	}{{"help-", len(out.Notes) > 0}, {"source-", out.Source != ""}, {"error-", out.Error != ""}} {
		if d.present {
			ids = append(ids, d.prefix+out.ID)
		}
	}
	out.DescribedBy = strings.Join(ids, " ")
	return out
}

func (b *builder) children(fields []settings.Field, path string, value any) []field {
	object, _ := value.(map[string]any)
	var out []field
	for _, child := range fields {
		out = append(out, b.field(child, path+"."+child.Name, object[child.Name], nil))
	}
	return out
}

func (b *builder) items(f settings.Field, path string, value any) []item {
	values, _ := value.([]any)
	n := len(values)
	if b.posted != nil {
		n = b.posted.Counts[path]
	}
	items := make([]item, n)
	for i := range items {
		var v any
		if i < len(values) {
			v = values[i]
		}
		p := fmt.Sprintf("%s[%d]", path, i)
		items[i] = item{Path: p, N: i + 1, First: i == 0, Last: i == n-1}
		if f.Items.Type == settings.Object {
			items[i].Children = b.children(f.Items.Fields, p, v)
			continue
		}
		simple := settings.Field{Label: f.Label, Type: f.Items.Type, Options: f.Items.Options, Editable: true}
		items[i].Field = b.field(simple, p, v, nil)
	}
	return items
}

// text is what the control at path shows: what was sent, else the document's value.
func (b *builder) text(path string, value any) string {
	if b.posted != nil {
		return b.posted.Texts[path]
	}
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func (b *builder) variables() []variableRow {
	var rows []variableRow
	for _, v := range b.view.Variables {
		row := variableRow{Name: v.Name, ID: variableID(v.Name), Password: v.Secret || v.Value == nil}
		if !row.Password {
			row.Value = *v.Value
			if b.posted != nil {
				row.Value = b.posted.Variables[v.Name]
			}
		}
		row.Source = "Set in .env. The hub does not show the value because it cannot tell whether it is a secret."
		if v.Saved {
			row.Source = sourceText(row.Password, v.Source, !row.Password && *v.Value == "")
			text := map[bool]string{true: "Use the .env value", false: "Remove"}[v.EnvSet]
			row.Back = &checkbox{Name: "var_unset", Value: v.Name, Text: text, Checked: b.posted != nil && b.posted.VarUnset[v.Name]}
		}
		ids := "source-var-" + row.ID
		if message, ok := b.errs["env:"+v.Name]; ok {
			row.Error, ids = message, ids+" error-var-"+row.ID
			b.drawn["env:"+v.Name] = "var-" + row.ID
		}
		row.DescribedBy = ids
		rows = append(rows, row)
	}
	return rows
}

// newRows give back what was sent, but a value only when nothing says it is secret.
func (b *builder) newRows() []newRowView {
	if b.posted == nil || len(b.posted.New) == 0 {
		return []newRowView{{}}
	}
	rows := make([]newRowView, len(b.posted.New))
	for i, r := range b.posted.New {
		rows[i] = newRowView{I: i, Name: r.Name, Secret: r.Secret}
		if !r.Secret && !b.secretName(r.Name) {
			rows[i].Value = r.Value
		}
	}
	return rows
}

func (b *builder) secretName(name string) bool {
	return slices.ContainsFunc(b.view.Variables, func(v monitorconfig.VariableView) bool {
		return v.Name == name && (v.Secret || v.Value == nil)
	}) || slices.ContainsFunc(b.view.Declaration.Fields, func(f settings.Field) bool {
		return f.Secret && f.Env == name
	}) || slices.ContainsFunc(b.view.Ignored, func(v monitorconfig.IgnoredView) bool {
		return v.Secret && (v.Path == name || v.Path == "env:"+name)
	})
}

func (b *builder) typedSecret() bool {
	if b.posted == nil {
		return false
	}
	if len(b.posted.Secrets) > 0 {
		return true
	}
	for _, r := range b.posted.New {
		if r.Value != "" && (r.Secret || b.secretName(r.Name)) {
			return true
		}
	}
	return slices.ContainsFunc(b.view.Variables, func(v monitorconfig.VariableView) bool {
		return (v.Secret || v.Value == nil) && b.posted.Variables[v.Name] != ""
	})
}

func (b *builder) ignored() []ignoredRow {
	reasons := map[monitorconfig.IgnoredReason]string{
		monitorconfig.ReasonUndeclared:    "The monitor no longer declares this field.",
		monitorconfig.ReasonType:          "The saved value no longer fits this field.",
		monitorconfig.ReasonNotEditable:   "The monitor does not allow changing this field here.",
		monitorconfig.ReasonNoDeclaration: "The monitor has no settings.json that this hub can read.",
		monitorconfig.ReasonReplaced:      "This value was saved before the field became secret. The secret saved later takes its place.",
	}
	var rows []ignoredRow
	for i, v := range b.view.Ignored {
		row := ignoredRow{I: i, Path: v.Path, Reason: reasons[v.Reason], Secret: v.Secret, Drop: b.posted != nil && b.posted.Drop[v.Path]}
		if !v.Secret {
			row.Value = indented(v.Value)
		}
		if message, ok := b.errs["ignored:"+v.Path]; ok {
			row.Error = message
			b.drawn["ignored:"+v.Path] = "ignored-" + strconv.Itoa(i)
		}
		rows = append(rows, row)
	}
	return rows
}

// summary lists every problem, linked to where it is drawn; it runs after everything is built.
func (b *builder) summary(problems []monitorconfig.Problem) []summaryLine {
	var lines []summaryLine
	for _, p := range problems {
		text := p.Message
		if p.Path != "" {
			text = p.Path + ": " + p.Message
		}
		lines = append(lines, summaryLine{Target: b.drawn[p.Path], Text: text})
	}
	return lines
}

func indented(raw json.RawMessage) string {
	var out bytes.Buffer
	if json.Indent(&out, raw, "", "  ") != nil {
		return string(raw)
	}
	return out.String()
}

func sourceText(secret bool, source monitorconfig.Source, empty bool) string {
	if source == monitorconfig.SourcePage && empty {
		return "Saved empty on this page"
	}
	texts := map[bool]map[monitorconfig.Source]string{
		false: {
			monitorconfig.SourcePage:    "Saved on this page",
			monitorconfig.SourceEnv:     "From .env",
			monitorconfig.SourceDefault: "Using the default",
			monitorconfig.SourceUnset:   "Not set",
		},
		true: {
			monitorconfig.SourcePage:    "Set on this page",
			monitorconfig.SourceEnv:     "Set in .env",
			monitorconfig.SourceDefault: "Empty",
			monitorconfig.SourceUnset:   "Empty",
		},
	}
	return texts[secret][source]
}

// backText names what a field goes back to once its saved value is taken out.
func backText(f settings.Field, envSet bool) string {
	if envSet {
		return "Use the .env value"
	}
	if f.Default != nil {
		return "Use the default"
	}
	return "Clear"
}

// defaultText is the default as a person reads it: text without quotes, a boolean as Yes or No.
func defaultText(f settings.Field) string {
	if f.Default == nil {
		return ""
	}
	var value any
	if json.Unmarshal(f.Default, &value) != nil {
		return string(f.Default)
	}
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return yesNo(v)
	}
	return string(f.Default)
}

func yesNo(b bool) string {
	return map[bool]string{true: "Yes", false: "No"}[b]
}

// selectOptions offers the empty choice only when there is no value, and a value that is not
// one of the options as one of its own.
func selectOptions(f settings.Field, value, shownDefault string) []selectOption {
	var choices []selectOption
	if f.Type == settings.Boolean {
		choices = []selectOption{{Value: "true", Text: yesNo(true)}, {Value: "false", Text: yesNo(false)}}
	}
	for _, o := range f.Options {
		choices = append(choices, selectOption{Value: o, Text: o})
	}
	var out []selectOption
	if value == "" {
		empty := "Not set"
		if shownDefault != "" {
			empty = "Default (" + shownDefault + ")"
		}
		out = append(out, selectOption{Text: empty, Selected: true})
	}
	known := false
	for _, c := range choices {
		c.Selected = c.Value == value
		known = known || c.Selected
		out = append(out, c)
	}
	if value != "" && !known {
		out = append(out, selectOption{Value: value, Text: value + " (not one of the options)", Selected: true})
	}
	return out
}

func notes(texts ...string) []string {
	return slices.DeleteFunc(texts, func(s string) bool { return s == "" })
}

// prefixed is prefix and text, or "" without text.
func prefixed(prefix, text string) string {
	if text == "" {
		return ""
	}
	return prefix + text
}
