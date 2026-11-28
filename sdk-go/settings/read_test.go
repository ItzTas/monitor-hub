package settings_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

func read(t *testing.T, file string) *settings.Declaration {
	t.Helper()
	declaration, err := settings.Read([]byte(file))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return declaration
}

func TestReadReturnsTheDeclaration(t *testing.T) {
	got := read(t, doc(
		`{"name":"mode","label":"Mode","help":"Pick one.","type":"choice","options":["a","b"],"default":"b","group":"G","required":true,"secret":false,"editable":false,"reason":"R","env":"MODE"}`,
		`{"name":"rules","label":"Rules","type":"list","items":{"type":"object","fields":[{"name":"process","label":"Process","type":"string"}]}}`,
		`{"name":"login","label":"Login","type":"object","fields":[{"name":"password","label":"Password","type":"string","secret":true}]}`,
		`{"name":"plain","label":"Plain","type":"string","default":null}`,
	))
	want := &settings.Declaration{Fields: []settings.Field{
		{
			Name: "mode", Label: "Mode", Help: "Pick one.", Type: settings.Choice, Options: []string{"a", "b"},
			Default: json.RawMessage(`"b"`), Group: "G", Required: true, Reason: "R", Env: "MODE",
		},
		{
			Name: "rules", Label: "Rules", Type: settings.List, Editable: true,
			Items: &settings.Item{Type: settings.Object, Fields: []settings.Field{
				{Name: "process", Label: "Process", Type: settings.String, Editable: true},
			}},
		},
		{
			Name: "login", Label: "Login", Type: settings.Object, Editable: true, Fields: []settings.Field{
				{Name: "password", Label: "Password", Type: settings.String, Secret: true, Editable: true},
			},
		},
		{Name: "plain", Label: "Plain", Type: settings.String, Editable: true},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read() = %+v, want %+v", got, want)
	}
}

func TestReadIsLenientWhereTheContractSaysSo(t *testing.T) {
	tests := []struct {
		name string
		file string
		want []settings.Field
	}{
		{
			"unknown attributes",
			`{"settings_version":2,"later":1,"fields":[{"name":"a","label":"A","type":"list","hint":"h","items":{"type":"string","pattern":"x"}}]}`,
			[]settings.Field{{Name: "a", Label: "A", Type: settings.List, Editable: true, Items: &settings.Item{Type: settings.String}}},
		},
		{
			"an unknown type",
			doc(`{"name":"a","label":"A","type":"future","options":["a","a"],"fields":1,"items":2}`),
			[]settings.Field{{Name: "a", Label: "A", Type: settings.String, Editable: true}},
		},
		{
			"an unknown type with a default",
			doc(`{"name":"a","label":"A","type":"future","default":5}`),
			[]settings.Field{{Name: "a", Label: "A", Type: settings.String, Editable: true}},
		},
		{
			"an unknown item type",
			doc(`{"name":"a","label":"A","type":"list","items":{"type":"future","options":["a","a"],"fields":1}}`),
			[]settings.Field{{Name: "a", Label: "A", Type: settings.List, Editable: true, Items: &settings.Item{Type: settings.String}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := read(t, tt.file); !reflect.DeepEqual(got.Fields, tt.want) {
				t.Fatalf("Read().Fields = %+v, want %+v", got.Fields, tt.want)
			}
		})
	}
}

func TestReadRefuses(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{"byte order mark", "\xef\xbb\xbf" + doc(okField), "settings.json starts with a UTF-8 byte order mark"},
		{"invalid UTF-8", second("{\"name\":\"b\",\"label\":\"\xff\",\"type\":\"string\"}"), "settings.json is not valid: the file is not UTF-8"},
		{"a repeated key", second(`{"name":"b","name":"c","label":"L","type":"string"}`), `settings.json: key "name" appears more than once in one object`},
		{"a key in another letter case", second(`{"name":"b","label":"L","type":"string","Env":"B"}`), `settings.json: key "Env" differs from "env" only in letter case`},
		{"null required", second(`{"name":"b","label":"L","type":"string","required":null}`), "settings.json: fields[1]: required must be true or false"},
		{"null secret", second(`{"name":"b","label":"L","type":"string","secret":null}`), "settings.json: fields[1]: secret must be true or false"},
		{"null editable", second(`{"name":"b","label":"L","type":"string","editable":null}`), "settings.json: fields[1]: editable must be true or false"},
		{"text editable", second(`{"name":"b","label":"L","type":"string","editable":"false"}`), "settings.json: fields[1]: editable must be true or false"},
		{"settings_version 3", `{"settings_version":3,"fields":[]}`, "settings.json: settings_version is 3, want 2"},
		{"settings_version as text", `{"settings_version":"2","fields":[]}`, `settings.json: settings_version is "2", want 2`},
		{"settings_version with a fraction", `{"settings_version":2.0,"fields":[]}`, "settings.json: settings_version is 2.0, want 2"},
		{"no settings_version", `{"fields":[]}`, "settings.json: settings_version is missing, want 2"},
		{
			"a broken rule",
			second(`{"name":"b","label":"L","type":"list","items":{"type":"object","fields":[{"name":"c","label":"C","type":"string","secret":true}]}}`),
			"settings.json: fields[1].items.fields[0]: a field inside a list item cannot be secret",
		},
		{
			"the first of two broken fields",
			doc(`{"name":"A","label":"A","type":"string"}`, `{"name":"b","label":" ","type":"string"}`),
			`settings.json: fields[0]: name "A" does not match ^[a-z][a-z0-9_]*$`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declaration, err := settings.Read([]byte(tt.file))
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Read() = %+v, %v, want error %q", declaration, err, tt.want)
			}
		})
	}
}

func TestDeclares(t *testing.T) {
	declaration := read(t, doc(
		`{"name":"sei_url","label":"SEI address","type":"string","required":true,"env":"SEI_URL"}`,
		`{"name": "rules", "label": "Notification rules", "type": "list",
		 "items": {"type": "object", "fields": [
		   {"name": "process", "label": "Process number (regular expression)", "type": "string", "required": true},
		   {"name": "actions", "label": "Actions", "type": "list", "items": {"type": "choice", "options": ["urgent", "ignore"]}}
		 ]}}`,
	))
	tests := map[string]bool{
		"":                                    true,
		"sei_url":                             true,
		"rules":                               true,
		"rules[0]":                            true,
		"rules[12].process":                   true,
		"rules[99999999999999999999].process": true,
		"rules[3].actions":                    true,
		"rules[3].actions[0]":                 true,
		"rules.process":                       false,
		"rules[01].process":                   false,
		"rules[].process":                     false,
		"rules[0].":                           false,
		"rules[-1]":                           false,
		"sei_url[0]":                          false,
		"rules[0].other":                      false,
		"SEI_URL":                             false,
		"[0]":                                 false,
		"sei_url.":                            false,
	}
	for path, want := range tests {
		if got := declaration.Declares(path); got != want {
			t.Errorf("Declares(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestReserved(t *testing.T) {
	tests := map[string]bool{
		"PATH":                  true,
		"TZ":                    true,
		"TMPDIR":                true,
		"PLUGIN_X":              true,
		"MONITOR_HUB_CONFIG_FD": true,
		"LD_PRELOAD":            true,
		"PATHS":                 false,
		"TZ_NAME":               false,
		"LD":                    false,
		"SEI_URL":               false,
	}
	for env, want := range tests {
		if got := settings.Reserved(env); got != want {
			t.Errorf("Reserved(%q) = %v, want %v", env, got, want)
		}
	}
}
