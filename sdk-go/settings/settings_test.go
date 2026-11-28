package settings_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

type vector struct {
	Name     string          `json:"name"`
	Settings json.RawMessage `json:"settings"`
	Problem  *string         `json:"problem"`
}

func readVectors(t *testing.T) []vector {
	t.Helper()
	data, err := os.ReadFile("../../testdata/config/declarations.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []vector `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file.Cases
}

func TestCheckFollowsTheSharedVectors(t *testing.T) {
	vectors := readVectors(t)
	if len(vectors) == 0 {
		t.Fatal("the vector file has no cases")
	}
	for _, tt := range vectors {
		t.Run(tt.Name, func(t *testing.T) {
			var want []string
			if tt.Problem != nil {
				want = []string{*tt.Problem}
			}
			if got := settings.Check(tt.Settings); !slices.Equal(got, want) {
				t.Fatalf("Check() = %q, want %q", got, want)
			}
		})
	}
}

const okField = `{"name":"ok","label":"OK","type":"string"}`

func doc(fields ...string) string {
	return `{"settings_version":2,"fields":[` + strings.Join(fields, ",") + `]}`
}

func second(field string) string {
	return doc(okField, field)
}

func TestCheckReportsEveryBrokenField(t *testing.T) {
	const typeList = "string, integer, number, boolean, duration, choice, object, list"
	tests := []struct {
		name string
		file string
		want []string
	}{
		{
			"two broken fields at different levels",
			doc(
				`{"name":"login","label":"Login","type":"object","fields":[{"name":"User","label":"User","type":"string"}]}`,
				`{"name":"b","label":" ","type":"string"}`,
			),
			[]string{
				`settings.json: fields[0].fields[0]: name "User" does not match ^[a-z][a-z0-9_]*$`,
				"settings.json: fields[1]: label is empty",
			},
		},
		{
			"a field past the depth limit, reported without what nests under it",
			doc(`{"name":"a","label":"A","type":"object","fields":[{"name":"b","label":"B","type":"object","fields":[{"name":"c","label":"C","type":"object","fields":[{"name":"d","label":"D","type":"object","fields":[{"name":"e","label":"E","type":"object","fields":[{"name":"F","label":"F","type":"string"}]}]}]}]}]}`),
			[]string{"settings.json: fields[0].fields[0].fields[0].fields[0].fields[0]: fields nest more than 4 levels deep"},
		},
		{
			"the first problem of a field in rule order",
			second(`{"name":"b","label":"L","type":"string","secret":true,"default":"x","options":["a"]}`),
			[]string{"settings.json: fields[1]: options are only for choice"},
		},
		{"options that differ only in letter case", second(`{"name":"b","label":"L","type":"choice","options":["a","A"]}`), nil},
		{"options that differ only in a space", second(`{"name":"b","label":"L","type":"choice","options":["a","a "]}`), nil},
		{"byte order mark", "\xef\xbb\xbf" + doc(okField), []string{"settings.json starts with a UTF-8 byte order mark"}},
		{"invalid UTF-8", second("{\"name\":\"b\",\"label\":\"\xff\",\"type\":\"string\"}"), []string{"settings.json is not valid: the file is not UTF-8"}},
		{
			"an attribute in another letter case",
			second(`{"name":"b","label":"L","type":"string","Secret":true}`),
			[]string{`settings.json: key "Secret" differs from "secret" only in letter case`},
		},
		{
			"a top-level attribute in another letter case",
			`{"Settings_Version":2,"fields":[]}`,
			[]string{`settings.json: key "Settings_Version" differs from "settings_version" only in letter case`},
		},
		{
			"an attribute in another letter case inside a list item",
			second(`{"name":"b","label":"L","type":"list","items":{"type":"object","fields":[{"name":"c","label":"C","type":"string","Secret":true}]}}`),
			[]string{`settings.json: key "Secret" differs from "secret" only in letter case`},
		},
		{
			"letter case outside settings attributes",
			second(`{"name":"b","label":"L","type":"string","default":{"Type":1}}`),
			[]string{"settings.json: fields[1]: default does not fit the field type"},
		},
		{
			"a repeated attribute",
			second(`{"name":"b","label":"L","type":"string","secret":true,"secret":false}`),
			[]string{`settings.json: key "secret" appears more than once in one object`},
		},
		{
			"a repeated key inside a default",
			second(`{"name":"b","label":"L","type":"string","default":{"x":1,"x":2}}`),
			[]string{`settings.json: key "x" appears more than once in one object`},
		},
		{
			"an unknown attribute",
			doc(`{"name":"login","label":"Login","type":"object","fields":[` + okField + `,{"name":"b","label":"L","type":"string","secert":true}]}`),
			[]string{`settings.json is not valid: fields[0].fields[1]: unknown attribute "secert"`},
		},
		{"an unknown top-level attribute", `{"settings_version":2,"fields":[],"extra":1}`, []string{`settings.json is not valid: unknown attribute "extra"`}},
		{
			"an unknown attribute in a list item",
			second(`{"name":"b","label":"L","type":"list","items":{"type":"string","label":"L"}}`),
			[]string{`settings.json is not valid: fields[1].items: unknown attribute "label"`},
		},
		{"an unknown type", second(`{"name":"b","label":"L","type":"password"}`), []string{`settings.json: fields[1]: type "password" is not one of ` + typeList}},
		{
			"an unknown type in a list item",
			second(`{"name":"b","label":"L","type":"list","items":{"type":"password"}}`),
			[]string{`settings.json: fields[1].items: type "password" is not one of ` + typeList},
		},
		{"null required", second(`{"name":"b","label":"L","type":"string","required":null}`), []string{"settings.json: fields[1]: required must be true or false"}},
		{"null secret", second(`{"name":"b","label":"L","type":"string","secret":null}`), []string{"settings.json: fields[1]: secret must be true or false"}},
		{"null editable", second(`{"name":"b","label":"L","type":"string","editable":null}`), []string{"settings.json: fields[1]: editable must be true or false"}},
		{"text secret", second(`{"name":"b","label":"L","type":"string","secret":"yes"}`), []string{"settings.json: fields[1]: secret must be true or false"}},
		{"settings_version 3", `{"settings_version":3,"fields":[]}`, []string{"settings.json: settings_version is 3, want 2"}},
		{"settings_version as text", `{"settings_version":"2","fields":[]}`, []string{`settings.json: settings_version is "2", want 2`}},
		{"settings_version with a fraction", `{"settings_version":2.0,"fields":[]}`, []string{"settings.json: settings_version is 2.0, want 2"}},
		{"no settings_version", `{"fields":[]}`, []string{"settings.json: settings_version is missing, want 2"}},
		{"null settings_version", `{"settings_version":null,"fields":[]}`, []string{"settings.json: settings_version is missing, want 2"}},
		{"no fields", `{"settings_version":2}`, []string{"settings.json: fields is missing"}},
		{"null fields", `{"settings_version":2,"fields":null}`, []string{"settings.json: fields is missing"}},
		{"no name", second(`{"label":"L","type":"string"}`), []string{"settings.json: fields[1]: name is missing"}},
		{"null name", second(`{"name":null,"label":"L","type":"string"}`), []string{"settings.json: fields[1]: name is missing"}},
		{"no label", second(`{"name":"b","type":"string"}`), []string{"settings.json: fields[1]: label is missing"}},
		{"null label", second(`{"name":"b","label":null,"type":"string"}`), []string{"settings.json: fields[1]: label is missing"}},
		{"no type", second(`{"name":"b","label":"L"}`), []string{"settings.json: fields[1]: type is missing"}},
		{"null type", second(`{"name":"b","label":"L","type":null}`), []string{"settings.json: fields[1]: type is missing"}},
		{"no type in a list item", second(`{"name":"b","label":"L","type":"list","items":{}}`), []string{"settings.json: fields[1].items: type is missing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := settings.Check([]byte(tt.file)); !slices.Equal(got, tt.want) {
				t.Fatalf("Check() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckReportsJSONErrors(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		prefix string
	}{
		{"a label that is a number", second(`{"name":"b","label":1,"type":"string"}`), "settings.json is not valid: fields[1].label: "},
		{"fields that are an object", `{"settings_version":2,"fields":{}}`, "settings.json is not valid: fields: "},
		{"a field that is a list", doc(`["a","A",null,"string"]`), "settings.json is not valid: fields[0]: "},
		{"items that are a list", second(`{"name":"b","label":"L","type":"list","items":["string"]}`), "settings.json is not valid: fields[1].items: "},
		{"not an object", `[]`, "settings.json is not valid: "},
		{"an empty file", ``, "settings.json is not valid: "},
		{"data after the object", doc() + `{}`, "settings.json is not valid: "},
		{"garbage after the object", doc() + `x`, "settings.json is not valid: "},
		{"a cut file", `{"settings_version":2,"fields":[`, "settings.json is not valid: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := settings.Check([]byte(tt.file))
			if len(got) != 1 || !strings.HasPrefix(got[0], tt.prefix) || len(got[0]) == len(tt.prefix) {
				t.Fatalf("Check() = %q, want one violation that starts with %q and has a detail", got, tt.prefix)
			}
		})
	}
}
