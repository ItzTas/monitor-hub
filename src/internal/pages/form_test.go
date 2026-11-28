package pages

import (
	"encoding/json"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

func TestParseSubmissionWalksTheDeclaration(t *testing.T) {
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))

	sub := parseSubmission(view, url.Values{
		"revision": {"7"}, "declaration": {"key"},
		"n:labels": {"3"}, "v:labels[0]": {"a"}, "v:labels[1]": {""}, "v:labels[2]": {"c"},
		"n:rules": {"2"}, "v:rules[0].process": {"p"}, "v:rules[0].limit": {"x"}, "v:rules[0].notify": {"true"},
		"n:rules[0].actions": {"1"}, "v:rules[0].actions[0]": {"urgent"},
		"v:rules[1].process": {"q"}, "v:rules[1].limit": {""}, "v:rules[1].notify": {""},
		"v:rules[5].process": {"z"},
		"v:account.user":     {"paulo-santos"}, "v:account.token": {"x"},
		"v:ratio": {"1.5"}, "v:enabled": {""}, "v:when": {""}, "v:count": {"2"}, "v:level": {""},
		"v:not_declared": {"x"}, "v:secret_key": {"x"},
		"s:account.token": {"typed"}, "s:secret_key": {""},
		"unset": {"rules", "when"}, "clear": {"account.token"}, "drop": {"old_name"},
		"var.FREE": {"w"}, "var_unset": {"FREE"},
		"new_name": {" HTTP_PROXY ", "API_KEY"}, "new_value": {"http://p", "typed-secret-one"}, "new_kind": {"plain", "secret"},
	})

	want := map[string]any{
		"labels": []any{"a", "c"},
		"rules": []any{
			map[string]any{"process": "p", "notify": true, "actions": []any{"urgent"}},
			map[string]any{"process": "q"},
		},
		"account": map[string]any{"user": "paulo-santos"},
		"ratio":   json.Number("1.5"),
		"when":    "",
		"count":   json.Number("2"),
	}
	if !reflect.DeepEqual(sub.Values, want) {
		t.Errorf("Values = %#v\nwant %#v", sub.Values, want)
	}
	if !slices.Equal(sub.Indexes["labels"], []int{0, 2}) || !slices.Equal(sub.Indexes["rules"], []int{0, 1}) {
		t.Errorf("Indexes = %v, want labels at 0 and 2 of the form and rules at 0 and 1", sub.Indexes)
	}
	if !slices.Equal(sub.Problems, []monitorconfig.Problem{{Path: "rules[0].limit", Message: "use a whole number"}}) {
		t.Errorf("Problems = %v, want only rules[0].limit with use a whole number", sub.Problems)
	}
	for path, want := range map[string]string{"rules[0].limit": "x", "labels[1]": "", "enabled": "", "ratio": "1.5", "rules[0].actions[0]": "urgent"} {
		if got, ok := sub.Texts[path]; !ok || got != want {
			t.Errorf("Texts[%s] = %q, %v, want %q", path, got, ok, want)
		}
	}
	for _, path := range []string{"rules[5].process", "not_declared", "secret_key", "account.token"} {
		if _, ok := sub.Texts[path]; ok {
			t.Errorf("Texts has %s, which the form does not draw", path)
		}
	}
	if !maps.Equal(sub.Counts, map[string]int{"labels": 3, "rules": 2, "rules[0].actions": 1}) {
		t.Errorf("Counts = %v", sub.Counts)
	}
	if len(sub.Secrets) != 1 || sub.Secrets["account.token"] != "typed" {
		t.Errorf("Secrets has %d values and account.token typed: %v, want only account.token", len(sub.Secrets), sub.Secrets["account.token"] == "typed")
	}
	if sub.Revision != "7" || sub.Declaration != "key" {
		t.Errorf("the hidden fields are %q and %q, want 7 and key", sub.Revision, sub.Declaration)
	}
	sets := map[string]map[string]bool{
		"Unset": sub.Unset, "Clear": sub.Clear, "Drop": sub.Drop, "VarUnset": sub.VarUnset,
	}
	wantSets := map[string]map[string]bool{
		"Unset": {"rules": true, "when": true}, "Clear": {"account.token": true}, "Drop": {"old_name": true}, "VarUnset": {"FREE": true},
	}
	for name, got := range sets {
		if !maps.Equal(got, wantSets[name]) {
			t.Errorf("%s = %v, want %v", name, got, wantSets[name])
		}
	}
	if !maps.Equal(sub.Variables, map[string]string{"FREE": "w"}) {
		t.Errorf("Variables = %v, want FREE w", sub.Variables)
	}
	if len(sub.New) != 2 || sub.New[0] != (newRow{Name: "HTTP_PROXY", Value: "http://p"}) || sub.New[1].Name != "API_KEY" || !sub.New[1].Secret {
		t.Errorf("New has %d rows, the first %+v, want HTTP_PROXY trimmed and a secret API_KEY", len(sub.New), sub.New[0])
	}
}

func TestParseSubmissionReadsAtMost1000Items(t *testing.T) {
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))

	sub := parseSubmission(view, url.Values{"n:labels": {"99999"}, "v:labels[999]": {"x"}, "v:labels[1000]": {"y"}})

	if !reflect.DeepEqual(sub.Values, map[string]any{"labels": []any{"x"}}) {
		t.Errorf("Values = %#v, want only labels, the one list sent", sub.Values)
	}
	if sub.Counts["labels"] != 1000 || !reflect.DeepEqual(sub.Values["labels"], []any{"x"}) || !slices.Equal(sub.Indexes["labels"], []int{999}) {
		t.Errorf("with n:labels=99999, Counts is %d, labels %v at %v, want 1000 items read and only x, at 999", sub.Counts["labels"], sub.Values["labels"], sub.Indexes["labels"])
	}
	broken := parseSubmission(view, url.Values{"n:labels": {"-3"}, "n:rules": {"many"}})
	if broken.Counts["labels"] != 0 || broken.Counts["rules"] != 0 {
		t.Errorf("Counts of a negative and a non number = %v, want 0 for both", broken.Counts)
	}
}

func TestParseSubmissionReadsOnlyWhatTheFormDraws(t *testing.T) {
	view := seiScene().view(declarationFrom(t, "sei-settings.json"))

	sub := parseSubmission(view, url.Values{"v:sei_url": {"https://sei.example.gov.br"}, "v:sei_password_file": {"/run/x"}, "v:sei_password": {"x"}})

	if !reflect.DeepEqual(sub.Values, map[string]any{"sei_url": "https://sei.example.gov.br"}) {
		t.Errorf("Values = %#v, want only sei_url: sei_password_file is not editable and sei_password is secret", sub.Values)
	}
}

func TestApplyOp(t *testing.T) {
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))
	rule := map[string][]string{"a": {"urgent"}, "b": {"ignore"}, "c": {"urgent", "ignore"}}
	form := func(order ...string) submission {
		values := url.Values{"n:rules": {strconv.Itoa(len(order))}, "n:labels": {"0"}}
		for i, name := range order {
			item := "rules[" + strconv.Itoa(i) + "]"
			values.Set("v:"+item+".process", name)
			values.Set("n:"+item+".actions", strconv.Itoa(len(rule[name])))
			for j, action := range rule[name] {
				values.Set("v:"+item+".actions["+strconv.Itoa(j)+"]", action)
			}
		}
		return parseSubmission(view, values)
	}
	base := form("a", "b", "c")
	withLabel := form("a", "b", "c")
	withLabel.Counts["labels"] = 1
	withAction := form("a", "b", "c")
	withAction.Counts["rules[1].actions"] = 2

	cases := map[string]submission{
		"remove:rules[1]":      form("a", "c"),
		"remove:rules[0]":      form("b", "c"),
		"up:rules[0]":          base,
		"up:rules[2]":          form("a", "c", "b"),
		"down:rules[1]":        form("a", "c", "b"),
		"down:rules[2]":        base,
		"add:labels":           withLabel,
		"add:rules[1].actions": withAction,
		"remove:rules[9]":      base,
		"add:rules[9].actions": base,
		"remove:rules":         base,
		"add:rules[0].process": base,
		"add:enabled":          base,
		"add:nope":             base,
		"up:labels[0]":         base,
		"remove:rules[-1]":     base,
		"down:rules[-1]":       base,
		"up:rules[x]":          base,
		"boom":                 base,
		"":                     base,
	}

	for op, want := range cases {
		got := applyOp(view.Declaration, base, op)
		if !maps.Equal(got.Texts, want.Texts) || !maps.Equal(got.Counts, want.Counts) {
			t.Errorf("applyOp(%q):\nTexts  %v\nCounts %v\nwant\nTexts  %v\nCounts %v", op, got.Texts, got.Counts, want.Texts, want.Counts)
		}
	}
	if fresh := form("a", "b", "c"); !maps.Equal(base.Texts, fresh.Texts) || !maps.Equal(base.Counts, fresh.Counts) {
		t.Error("applyOp changed the submission it was given")
	}
}
