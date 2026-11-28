package monitorconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// plain is an IgnoredValue with its value as compact text, so a failure prints readable JSON.
type plain struct {
	Path   string
	Value  string
	Secret bool
	Reason IgnoredReason
}

func plainIgnored(ignored []IgnoredValue) []plain {
	out := make([]plain, 0, len(ignored))
	for _, v := range ignored {
		out = append(out, plain{v.Path, string(v.Value), v.Secret, v.Reason})
	}
	return out
}

// shownIgnored hides the value of every secret entry.
func shownIgnored(ignored []plain) []plain {
	out := slices.Clone(ignored)
	for i := range out {
		if out[i].Secret {
			out[i].Value = "redacted"
		}
	}
	return out
}

func TestAcceptKeepsWhatTheDeclarationTakes(t *testing.T) {
	const (
		str      = `[{"name":"f","label":"F","type":"string"}]`
		duration = `[{"name":"f","label":"F","type":"duration"}]`
		integer  = `[{"name":"f","label":"F","type":"integer"}]`
		number   = `[{"name":"f","label":"F","type":"number"}]`
		boolean  = `[{"name":"f","label":"F","type":"boolean"}]`
		choice   = `[{"name":"f","label":"F","type":"choice","options":["full","summary"]}]`
		secret   = `[{"name":"f","label":"F","type":"string","secret":true}]`
		list     = `[{"name":"l","label":"L","type":"list","items":{"type":"string"}}]`
		object   = `[{"name":"o","label":"O","type":"object","fields":[{"name":"a","label":"A","type":"string"},{"name":"p","label":"P","type":"string","secret":true}]}]`
		rules    = `[{"name":"r","label":"R","type":"list","items":{"type":"object","fields":[{"name":"a","label":"A","type":"string"}]}}]`
	)
	refused := func(path, value string, secret bool) []plain {
		return []plain{{path, value, secret, ReasonType}}
	}
	tests := []struct {
		name    string
		fields  string
		raw     string
		want    string
		ignored []plain
	}{
		{"string", str, `"a"`, `"a"`, nil},
		{"string given a number", str, `5`, ``, refused("f", `5`, false)},
		{"duration", duration, `"15m"`, `"15m"`, nil},
		{"duration given a boolean", duration, `true`, ``, refused("f", `true`, false)},
		{"integer", integer, `3`, `3`, nil},
		{"integer with a fraction", integer, `3.0`, ``, refused("f", `3.0`, false)},
		{"integer with an exponent", integer, `1e2`, ``, refused("f", `1e2`, false)},
		{"integer beyond int64", integer, `9223372036854775808`, ``, refused("f", `9223372036854775808`, false)},
		{"integer negative zero", integer, `-0`, ``, refused("f", `-0`, false)},
		{"integer given text", integer, `"3"`, ``, refused("f", `"3"`, false)},
		{"number", number, `1.5`, `1.5`, nil},
		{"number without a fraction", number, `3`, `3`, nil},
		{"number beyond float64", number, `1e999`, ``, refused("f", `1e999`, false)},
		{"number given text", number, `"1.5"`, ``, refused("f", `"1.5"`, false)},
		{"boolean", boolean, `false`, `false`, nil},
		{"boolean given text", boolean, `"true"`, ``, refused("f", `"true"`, false)},
		{"choice", choice, `"full"`, `"full"`, nil},
		{"choice outside the options", choice, `"FULL"`, ``, refused("f", `"FULL"`, false)},
		{"null", str, `null`, ``, refused("f", `null`, false)},
		{"secret of the wrong type", secret, `5`, ``, refused("f", `5`, true)},
		{"list", list, `["a","b"]`, `["a","b"]`, nil},
		{"list with a bad item in the middle", list, `["a",5,"b"]`, `["a","b"]`, refused("l[1]", `5`, false)},
		{"list with a null item", list, `["a",null]`, `["a"]`, refused("l[1]", `null`, false)},
		{"list given text", list, `"a"`, ``, refused("l", `"a"`, false)},
		{"object", object, `{"a":"x"}`, `{"a":"x"}`, nil},
		{"object with an unknown key", object, `{"a":"x","z":1}`, `{"a":"x"}`, []plain{{"o.z", `1`, false, ReasonUndeclared}}},
		{"object with a child of the wrong type", object, `{"a":5}`, `{}`, refused("o.a", `5`, false)},
		{"object with a null child", object, `{"a":null}`, `{}`, refused("o.a", `null`, false)},
		{"object with a secret child of the wrong type", object, `{"p":5}`, `{}`, refused("o.p", `5`, true)},
		{"object given a list", object, `["a"]`, ``, refused("o", `["a"]`, false)},
		{"list of objects", rules, `[{"a":"x","q":1},3]`, `[{"a":"x"}]`, []plain{
			{"r[0].q", `1`, false, ReasonUndeclared},
			{"r[1]", `3`, false, ReasonType},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := declaration(t, tt.fields).Fields[0]
			value, ignored := Accept(field, field.Name, json.RawMessage(tt.raw))
			if got := plainIgnored(ignored); !reflect.DeepEqual(got, tt.ignored) && (len(got) > 0 || len(tt.ignored) > 0) {
				t.Errorf("ignored = %+v, want %+v", shownIgnored(got), shownIgnored(tt.ignored))
			}
			if tt.want == "" {
				if value != nil {
					t.Fatalf("value = %v, want none", value)
				}
				return
			}
			got, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("value = %s, want %s", got, tt.want)
			}
		})
	}
}
