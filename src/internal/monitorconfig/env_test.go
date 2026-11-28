package monitorconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

func declaration(t *testing.T, fields string) *settings.Declaration {
	t.Helper()
	decl, err := settings.Read([]byte(`{"settings_version":2,"fields":` + fields + `}`))
	if err != nil {
		t.Fatalf("read the declaration: %v", err)
	}
	return decl
}

func readCases[T any](t *testing.T, path string) []T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct{ Cases []T }
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file.Cases
}

// byValue decodes JSON with numbers as float64, so 3 and 3.0 compare equal.
func byValue(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return v
}

func valueOf(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return byValue(t, data)
}

func decodeDocument(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func hasSecret(decl *settings.Declaration) bool {
	return slices.ContainsFunc(decl.Fields, func(f settings.Field) bool { return f.Secret })
}

// shown keeps a value out of a failure message when the case holds a secret.
func shown(secret bool, v any) any {
	if secret {
		return "redacted"
	}
	return v
}

func TestEnvBridgeFollowsTheSharedVectors(t *testing.T) {
	type envCase struct {
		Name     string
		Fields   json.RawMessage
		Env      map[string]string
		Document json.RawMessage
		Problems []Problem
	}
	for _, c := range readCases[envCase](t, "../../../testdata/config/env.json") {
		t.Run(c.Name, func(t *testing.T) {
			decl := declaration(t, string(c.Fields))
			secret := hasSecret(decl)
			doc, problems := FromEnv(decl, func(name string) (string, bool) {
				text, ok := c.Env[name]
				return text, ok
			})
			if !slices.Equal(problems, c.Problems) {
				t.Fatalf("problems = %q, want %q", shown(secret, problems), shown(secret, c.Problems))
			}
			if c.Problems != nil {
				if doc != nil {
					t.Fatalf("document = %v, want none", shown(secret, doc))
				}
				return
			}
			got, want := valueOf(t, WithDefaults(decl, doc)), byValue(t, c.Document)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("document = %v, want %v", shown(secret, got), shown(secret, want))
			}
		})
	}
}

func TestWithDefaultsFollowsTheSharedVectors(t *testing.T) {
	type defaultsCase struct {
		Name     string
		Fields   json.RawMessage
		Document json.RawMessage
		Want     json.RawMessage
	}
	for _, c := range readCases[defaultsCase](t, "../../../testdata/config/defaults.json") {
		t.Run(c.Name, func(t *testing.T) {
			decl := declaration(t, string(c.Fields))
			got, want := valueOf(t, WithDefaults(decl, decodeDocument(t, c.Document))), byValue(t, c.Want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("document = %v, want %v", got, want)
			}
		})
	}
}

func fromOneVariable(t *testing.T, fields, text string) (map[string]any, []Problem) {
	t.Helper()
	return FromEnv(declaration(t, fields), func(string) (string, bool) { return text, true })
}

func TestTextThatIsNotUTF8(t *testing.T) {
	_, problems := fromOneVariable(t, `[{"name":"p","label":"P","type":"string","secret":true,"env":"P"}]`, "a\xffb")
	if want := []Problem{{"p", "P is not valid UTF-8"}}; !slices.Equal(problems, want) {
		t.Errorf("secret problems = %q, want %q", shown(true, problems), want)
	}
	// One U+FFFD per maximal invalid subpart, as Rust's String::from_utf8_lossy.
	tests := map[string]string{
		"a\xffb":         "a�b",
		"a\xe2\x82b":     "a�b",
		"a\xff\xffb":     "a��b",
		"a\xed\xa0\x80b": "a���b",
		"a\xe0\x80\x80b": "a���b",
		"a\xc0\xafb":     "a��b",
		"a\xf4\xbfb":     "a��b",
		"a\xf0\x9f\x98b": "a�b",
		"a\xf1\x80b":     "a�b",
		"a\xef\xbf\xbdb": "a�b",
	}
	for text, want := range tests {
		doc, problems := fromOneVariable(t, `[{"name":"s","label":"S","type":"string","env":"S"}]`, text)
		if problems != nil || doc["s"] != want {
			t.Errorf("%q gives %q and %q, want %q", text, doc["s"], problems, want)
		}
	}
}

func TestQuotedTextKeepsABackslash(t *testing.T) {
	// The encoder's own \u2028 escapes become the raw character, as serde_json writes it; a
	// backslash that belongs to the text stays.
	tests := map[string]string{
		`a\u2028b`:    `N is not an integer ("a\\u2028b")`,
		"a\u2028b":    "N is not an integer (\"a\u2028b\")",
		"a\\\u2028b":  "N is not an integer (\"a\\\\\u2028b\")",
		"a\\\\\u2029": "N is not an integer (\"a\\\\\\\\\u2029\")",
	}
	for text, want := range tests {
		_, problems := fromOneVariable(t, `[{"name":"n","label":"N","type":"integer","env":"N"}]`, text)
		if len(problems) != 1 || problems[0].Message != want {
			t.Errorf("%q gives %q, want %q", text, problems, want)
		}
	}
}

func TestConvertText(t *testing.T) {
	tests := []struct {
		kind     settings.Type
		text     string
		value    any
		expected string
	}{
		{settings.Integer, "+3", json.Number("3"), ""},
		{settings.Integer, "x", nil, "use a whole number"},
		{settings.Number, "1.50", json.Number("1.5"), ""},
		{settings.Number, "0x1p-2", nil, "use a number"},
		{settings.Boolean, "True", nil, "use true or false"},
		{settings.Boolean, "false", false, ""},
		{settings.String, "  ", "  ", ""},
		{settings.Duration, " 10m ", " 10m ", ""},
		{settings.Choice, "Other", "Other", ""},
	}
	for _, tt := range tests {
		value, expected := ConvertText(tt.kind, tt.text)
		if value != tt.value || expected != tt.expected {
			t.Errorf("ConvertText(%s, %q) = %#v, %q; want %#v, %q", tt.kind, tt.text, value, expected, tt.value, tt.expected)
		}
	}
}

func TestExpected(t *testing.T) {
	tests := []struct {
		field settings.Field
		want  string
	}{
		{settings.Field{Type: settings.String}, "use text"},
		{settings.Field{Type: settings.Duration}, "use text"},
		{settings.Field{Type: settings.Integer}, "use a whole number"},
		{settings.Field{Type: settings.Number}, "use a number"},
		{settings.Field{Type: settings.Boolean}, "use true or false"},
		{settings.Field{Type: settings.Choice, Options: []string{"full", "summary"}}, "use one of: full, summary"},
		{settings.Field{Type: settings.List}, "use a list"},
		{settings.Field{Type: settings.Object}, "use an object"},
	}
	for _, tt := range tests {
		if got := Expected(tt.field); got != tt.want {
			t.Errorf("Expected(%s) = %q, want %q", tt.field.Type, got, tt.want)
		}
	}
}

func TestListPathsCountEveryPieceKept(t *testing.T) {
	_, problems := fromOneVariable(t, `[{"name":"n","label":"N","type":"list","items":{"type":"integer"},"env":"N"}]`, "x, ,1,y")
	want := []Problem{{"n[0]", `N is not an integer ("x")`}, {"n[2]", `N is not an integer ("y")`}}
	if !slices.Equal(problems, want) {
		t.Errorf("problems = %q, want %q", problems, want)
	}
}
