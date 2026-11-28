package pages

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

// changeView is nested-settings.json with ratio 1.5 and when " 10m " from the .env, enabled from
// its default, level HIGH from the .env (not one of the options), labels, count, account (with a
// saved token) and one rule without actions saved, the variables FREE (saved) and PROXY (only in
// the .env), and old_name ignored.
func changeView(t *testing.T) monitorconfig.View {
	t.Helper()
	sc := scene{
		document: map[string]any{
			"account": map[string]any{"user": "paulo-santos"},
			"rules":   []any{map[string]any{"process": "a"}},
			"labels":  []any{"a", "b"},
			"count":   json.Number("4"),
			"ratio":   json.Number("1.5"),
			"enabled": true,
			"when":    " 10m ",
			"level":   "HIGH",
		},
		sources: map[string]monitorconfig.Source{
			"account": monitorconfig.SourcePage, "labels": monitorconfig.SourcePage, "ratio": monitorconfig.SourceEnv,
			"enabled": monitorconfig.SourceDefault, "when": monitorconfig.SourceEnv, "level": monitorconfig.SourceEnv,
			"secret_key": monitorconfig.SourceEnv, "rules": monitorconfig.SourcePage, "count": monitorconfig.SourcePage,
		},
		env:   []string{"ratio", "when", "level", "secret_key"},
		saved: []string{"account", "rules", "labels", "count", "account.token"},
	}
	view := sc.view(declarationFrom(t, "nested-settings.json"))
	free := "v"
	view.Variables = []monitorconfig.VariableView{
		{Name: "FREE", Source: monitorconfig.SourcePage, Value: &free, Saved: true},
		{Name: "PROXY", Secret: true, Source: monitorconfig.SourceEnv, EnvSet: true},
	}
	view.Ignored = []monitorconfig.IgnoredView{{Path: "old_name", Value: json.RawMessage(`1`), Reason: monitorconfig.ReasonUndeclared}}
	return view
}

// formOf is the form drawn for view, as a browser sends it.
func formOf(t *testing.T, view monitorconfig.View) url.Values {
	t.Helper()
	return drawnForm(renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, nil, nil)))
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func ptr(s string) *string { return &s }

// shownChange names the secrets of a change by path only, for failure messages.
func shownChange(c monitorconfig.Change) string {
	secrets := map[string]string{}
	for path, v := range c.Secrets {
		secrets[path] = map[bool]string{true: "cleared", false: "typed"}[v == nil]
	}
	document := map[string]string{}
	for k, v := range c.Document {
		document[k] = string(v)
	}
	set := slices.Sorted(func(yield func(string) bool) {
		for k := range c.Variables.Set {
			if !yield(k) {
				return
			}
		}
	})
	revision := "nil"
	if c.Revision != nil {
		revision = fmt.Sprint(*c.Revision)
	}
	return fmt.Sprintf("document %v, secrets %v, variables set %v unset %v secret %v, delete %v, revision %s",
		document, secrets, set, c.Variables.Unset, c.Variables.Secret, c.DeleteIgnored, revision)
}

func TestChangeFromTheForm(t *testing.T) {
	view := changeView(t)
	revision := int64(7)
	base := func(c monitorconfig.Change) monitorconfig.Change {
		c.Revision = &revision
		return c
	}
	choose := []monitorconfig.Problem{{Path: "level", Message: "Choose one of the options."}}
	cases := []struct {
		name     string
		edit     func(url.Values)
		want     monitorconfig.Change
		problems []monitorconfig.Problem
	}{
		{"the form as drawn", func(url.Values) {}, base(monitorconfig.Change{}), nil},
		{"ratio written as 1.50", func(f url.Values) { f.Set("v:ratio", "1.50") }, base(monitorconfig.Change{}), nil},
		{"when as the .env has it", func(f url.Values) { f.Set("v:when", " 10m ") }, base(monitorconfig.Change{}), nil},
		{"a third label", func(f url.Values) { f.Set("n:labels", "3"); f.Set("v:labels[2]", "c") },
			base(monitorconfig.Change{Document: map[string]json.RawMessage{"labels": raw(`["a","b","c"]`)}}), nil},
		{"another user", func(f url.Values) { f.Set("v:account.user", "psantos") },
			base(monitorconfig.Change{Document: map[string]json.RawMessage{"account": raw(`{"user":"psantos"}`)}}), nil},
		{"unset labels", func(f url.Values) { f.Add("unset", "labels") },
			base(monitorconfig.Change{Document: map[string]json.RawMessage{"labels": raw(`null`)}}), nil},
		{"unset ratio, which is not saved", func(f url.Values) { f.Add("unset", "ratio") }, base(monitorconfig.Change{}), nil},
		{"ratio emptied, not saved", func(f url.Values) { f.Set("v:ratio", "") }, base(monitorconfig.Change{}), nil},
		{"count emptied, saved", func(f url.Values) { f.Set("v:count", "") },
			base(monitorconfig.Change{Document: map[string]json.RawMessage{"count": raw(`null`)}}), nil},
		{"count written as 04", func(f url.Values) { f.Set("v:count", "04") }, base(monitorconfig.Change{}), nil},
		{"every label emptied", func(f url.Values) { f.Set("v:labels[0]", ""); f.Set("v:labels[1]", "") },
			base(monitorconfig.Change{Document: map[string]json.RawMessage{"labels": raw(`[]`)}}), nil},
		{"the token typed", func(f url.Values) { f.Set("s:account.token", "t") },
			base(monitorconfig.Change{Secrets: map[string]*string{"account.token": ptr("t")}}), nil},
		{"the token cleared", func(f url.Values) { f.Add("clear", "account.token") },
			base(monitorconfig.Change{Secrets: map[string]*string{"account.token": nil}}), nil},
		{"the token cleared and typed", func(f url.Values) { f.Add("clear", "account.token"); f.Set("s:account.token", "t") },
			base(monitorconfig.Change{Secrets: map[string]*string{"account.token": nil}}), nil},
		{"a secret path nothing declares", func(f url.Values) { f.Set("s:nope", "t") }, base(monitorconfig.Change{}), nil},
		{"secret_key, which is not editable, typed", func(f url.Values) { f.Set("s:secret_key", "t") }, base(monitorconfig.Change{}), nil},
		{"old_name dropped", func(f url.Values) { f.Add("drop", "old_name") },
			base(monitorconfig.Change{DeleteIgnored: []string{"old_name"}}), nil},
		{"a path nothing ignores dropped", func(f url.Values) { f.Add("drop", "nope") }, base(monitorconfig.Change{}), nil},
		{"secret_key sent as a value", func(f url.Values) { f.Set("v:secret_key", "x") }, base(monitorconfig.Change{}), nil},
		{"level loud", func(f url.Values) { f.Set("v:level", "loud") }, base(monitorconfig.Change{}), choose},
		{"level low", func(f url.Values) { f.Set("v:level", "low") },
			base(monitorconfig.Change{Document: map[string]json.RawMessage{"level": raw(`"low"`)}}), nil},
		{"a rule with an action that is not an option", func(f url.Values) {
			f.Set("n:rules", "1")
			f.Set("v:rules[0].process", "x")
			f.Set("n:rules[0].actions", "1")
			f.Set("v:rules[0].actions[0]", "loud")
		}, base(monitorconfig.Change{}), []monitorconfig.Problem{{Path: "rules[0].actions[0]", Message: "Choose one of the options."}}},
		{"a rule", func(f url.Values) {
			f.Set("n:rules", "1")
			f.Set("v:rules[0].process", "x")
			f.Set("n:rules[0].actions", "1")
			f.Set("v:rules[0].actions[0]", "urgent")
		}, base(monitorconfig.Change{Document: map[string]json.RawMessage{"rules": raw(`[{"actions":["urgent"],"process":"x"}]`)}}), nil},
		{"a count that is not a number", func(f url.Values) { f.Set("v:count", "three") }, base(monitorconfig.Change{}),
			[]monitorconfig.Problem{{Path: "count", Message: "use a whole number"}}},
		{"FREE changed", func(f url.Values) { f.Set("var.FREE", "w") },
			base(monitorconfig.Change{Variables: monitorconfig.VariableChange{Set: map[string]string{"FREE": "w"}}}), nil},
		{"FREE removed with a new text", func(f url.Values) { f.Set("var.FREE", "w"); f.Add("var_unset", "FREE") },
			base(monitorconfig.Change{Variables: monitorconfig.VariableChange{Unset: []string{"FREE"}}}), nil},
		{"PROXY removed, which is not saved", func(f url.Values) { f.Add("var_unset", "PROXY") }, base(monitorconfig.Change{}), nil},
		{"PROXY typed", func(f url.Values) { f.Set("var.PROXY", "p") },
			base(monitorconfig.Change{Variables: monitorconfig.VariableChange{Set: map[string]string{"PROXY": "p"}, Secret: []string{"PROXY"}}}), nil},
		{"a variable nothing shows", func(f url.Values) { f.Set("var.OTHER", "x") }, base(monitorconfig.Change{}), nil},
		{"new rows", func(f url.Values) {
			f["new_name"] = []string{"HTTP_PROXY", "API_KEY", ""}
			f["new_value"] = []string{"http://p", "s", ""}
			f["new_kind"] = []string{"plain", "secret", "plain"}
		}, base(monitorconfig.Change{Variables: monitorconfig.VariableChange{
			Set: map[string]string{"HTTP_PROXY": "http://p", "API_KEY": "s"}, Secret: []string{"API_KEY"},
		}}), nil},
		{"a new row with a value and no name", func(f url.Values) {
			f["new_name"], f["new_value"], f["new_kind"] = []string{""}, []string{"v"}, []string{"plain"}
		}, base(monitorconfig.Change{}), []monitorconfig.Problem{{Path: "", Message: "This new variable has a value but no name."}}},
		{"two new rows with one name", func(f url.Values) {
			f["new_name"], f["new_value"], f["new_kind"] = []string{"A", "A"}, []string{"1", "2"}, []string{"plain", "plain"}
		}, base(monitorconfig.Change{}), []monitorconfig.Problem{{Path: "A", Message: "Two new rows use this name."}}},
	}

	for _, c := range cases {
		form := formOf(t, view)
		c.edit(form)
		change, problems, err := changeFrom(view, parseSubmission(view, form))
		if err != nil {
			t.Errorf("%s: changeFrom failed: %v", c.name, err)
			continue
		}
		if len(problems) > 0 || len(c.problems) > 0 {
			if !slices.Equal(problems, c.problems) {
				t.Errorf("%s: the problems are %v, want %v", c.name, problems, c.problems)
			}
		}
		if !reflect.DeepEqual(change, c.want) {
			t.Errorf("%s: the change is\n%s\nwant\n%s", c.name, shownChange(change), shownChange(c.want))
		}
	}

	bare := changeView(t)
	delete(bare.Document, "labels")
	delete(bare.Document, "rules")
	for i := range bare.Secrets {
		bare.Secrets[i].Saved = false
	}
	form := formOf(t, bare)
	form.Add("clear", "account.token")
	if change, _, _ := changeFrom(bare, parseSubmission(bare, form)); !reflect.DeepEqual(change, base(monitorconfig.Change{})) {
		t.Errorf("the form as drawn without labels and rules, with the unsaved token cleared, changes %s, want nothing", shownChange(change))
	}

	form = formOf(t, view)
	form.Set("revision", "abc")
	if _, _, err := changeFrom(view, parseSubmission(view, form)); err == nil {
		t.Error("a revision that is not a number gives no error")
	}
}

func TestChangeFromLeavesOutListsTooLongToEdit(t *testing.T) {
	view := changeView(t)
	view.Document["labels"] = longList(maxItems + 1)

	form := formOf(t, view)
	if named(renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, nil, nil)), "n:labels") != "" {
		t.Error("the form of a configuration with 1001 labels draws them")
	}
	form.Set("v:ratio", "2")
	form.Set("n:labels", "1")
	form.Set("v:labels[0]", "a")
	form.Add("unset", "labels")
	change, problems, err := changeFrom(view, parseSubmission(view, form))

	want := monitorconfig.Change{Document: map[string]json.RawMessage{"ratio": raw(`2`)}, Revision: change.Revision}
	if err != nil || len(problems) > 0 || !reflect.DeepEqual(change, want) {
		t.Errorf("a change with 1001 labels saved = %s, %v, %v; want only ratio", shownChange(change), problems, err)
	}
}

func longList(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = fmt.Sprintf("term %d", i)
	}
	return items
}

func TestChangeFromEditsListsOf1000ItemsInAll(t *testing.T) {
	view := changeView(t)
	view.Document["labels"] = longList(maxItems - 1) // and the one rule

	form := formOf(t, view)
	form.Add("unset", "labels")
	change, _, err := changeFrom(view, parseSubmission(view, form))

	if err != nil || string(change.Document["labels"]) != "null" {
		t.Errorf("with 1000 list items in all, unsetting labels changes %s, want labels set to null", shownChange(change))
	}
}
