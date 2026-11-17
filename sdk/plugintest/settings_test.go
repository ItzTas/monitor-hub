package plugintest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const designExample = `{
  "settings_version": 1,
  "fields": [
    {
      "name": "WATCH_TERMS",
      "label": "Watch terms",
      "help": "Names or registration numbers to look for in each edition, separated by commas.",
      "type": "list",
      "required": true
    },
    {
      "name": "CHECK_INTERVAL_MINUTES",
      "label": "Check interval (minutes)",
      "type": "number",
      "default": "30"
    },
    {
      "name": "DATABASE_URL",
      "label": "Database address",
      "type": "string",
      "secret": true,
      "required": true
    }
  ]
}`

func fieldJSON(name string) string {
	return fmt.Sprintf(`{"name":%q,"label":"L","type":"string"}`, name)
}

func settingsDoc(fields ...string) string {
	return `{"settings_version":1,"fields":[` + strings.Join(fields, ",") + `]}`
}

func badSecond(bad string) string {
	return settingsDoc(fieldJSON("OK_FIELD"), bad)
}

func atSecond(text string) []string {
	return []string{"settings.json: fields[1]: " + text}
}

func TestSettingsViolations(t *testing.T) {
	const nameRule = ` does not match ^[A-Z][A-Z0-9_]*$`
	const reserved = ` is reserved by the hub`
	tests := []struct {
		name string
		file string
		want []string
	}{
		{"design example", designExample, nil},
		{"empty field list", `{"settings_version":1,"fields":[]}`, nil},
		{"secret choice", settingsDoc(`{"name":"B","label":"L","type":"choice","secret":true,"options":["a"]}`), nil},
		{"null optional attributes", settingsDoc(`{"name":"A","label":"L","type":"string","help":null,"default":null,"options":null,"group":null,"reason":null}`), nil},
		{"all attributes", settingsDoc(`{"name":"MODE","label":"Mode","help":"Pick one.","type":"choice","options":["a","b"],"default":"b","group":"G","required":true,"editable":false,"reason":"R"}`), nil},
		{"bom", "\xef\xbb\xbf" + designExample, []string{"settings.json starts with a UTF-8 byte order mark"}},
		{"invalid utf-8", "{\"settings_version\":1,\"fields\":[{\"name\":\"A\",\"label\":\"\xff\",\"type\":\"string\"}]}", []string{"settings.json is not valid: the file is not UTF-8"}},
		{"trailing data", designExample + "{}", []string{"settings.json is not valid: trailing data after the JSON object"}},
		{"trailing garbage", designExample + "x", []string{"settings.json is not valid: trailing data after the JSON object"}},
		{"unknown top-level attribute", `{"settings_version":1,"fields":[],"extra":1}`, []string{`settings.json is not valid: json: unknown field "extra"`}},
		{"unknown attribute", settingsDoc(`{"name":"A","label":"L","type":"string","secert":true}`), []string{`settings.json is not valid: json: unknown field "secert"`}},
		{"attribute in another case", settingsDoc(`{"name":"A","label":"L","type":"string","Secret":true}`), []string{`settings.json is not valid: json: unknown field "Secret"`}},
		{"top-level attribute in another case", `{"Settings_Version":1,"fields":[]}`, []string{`settings.json is not valid: json: unknown field "Settings_Version"`}},
		{"null secret", settingsDoc(`{"name":"A","label":"L","type":"string","secret":null}`), []string{"settings.json is not valid: secret must be true or false"}},
		{"null required", settingsDoc(`{"name":"A","label":"L","type":"string","required":null}`), []string{"settings.json is not valid: required must be true or false"}},
		{"null editable", settingsDoc(`{"name":"A","label":"L","type":"string","editable":null}`), []string{"settings.json is not valid: editable must be true or false"}},
		{"string secret", settingsDoc(`{"name":"A","label":"L","type":"string","secret":"yes"}`), []string{"settings.json is not valid: secret must be true or false"}},
		{"settings_version 2", `{"settings_version":2,"fields":[]}`, []string{"settings.json: settings_version is 2, want 1"}},
		{"settings_version 0", `{"settings_version":0,"fields":[]}`, []string{"settings.json: settings_version is 0, want 1"}},
		{"no settings_version", `{"fields":[]}`, []string{"settings.json: settings_version is missing"}},
		{"null settings_version", `{"settings_version":null,"fields":[]}`, []string{"settings.json: settings_version is missing"}},
		{"no fields", `{"settings_version":1}`, []string{"settings.json: fields is missing"}},
		{"null fields", `{"settings_version":1,"fields":null}`, []string{"settings.json: fields is missing"}},
		{"no name", badSecond(`{"label":"L","type":"string"}`), atSecond("name is missing")},
		{"null name", badSecond(`{"name":null,"label":"L","type":"string"}`), atSecond("name is missing")},
		{"no label", badSecond(`{"name":"B","type":"string"}`), atSecond("label is missing")},
		{"null label", badSecond(`{"name":"B","label":null,"type":"string"}`), atSecond("label is missing")},
		{"no type", badSecond(`{"name":"B","label":"L"}`), atSecond("type is missing")},
		{"null type", badSecond(`{"name":"B","label":"L","type":null}`), atSecond("type is missing")},
		{"unknown type", badSecond(`{"name":"B","label":"L","type":"password"}`), atSecond(`type "password" is not one of string, list, integer, number, duration, choice`)},
		{"lowercase name", badSecond(fieldJSON("watch_terms")), atSecond(`name "watch_terms"` + nameRule)},
		{"digit first", badSecond(fieldJSON("9X")), atSecond(`name "9X"` + nameRule)},
		{"empty name", badSecond(fieldJSON("")), atSecond(`name ""` + nameRule)},
		{"dash in name", badSecond(fieldJSON("A-B")), atSecond(`name "A-B"` + nameRule)},
		{"space in name", badSecond(fieldJSON("A B")), atSecond(`name "A B"` + nameRule)},
		{"non-ascii letter in name", badSecond(fieldJSON("AÉ")), atSecond(`name "AÉ"` + nameRule)},
		{"trailing newline in name", badSecond(`{"name":"A\n","label":"L","type":"string"}`), atSecond(`name "A\n"` + nameRule)},
		{"reserved PATH", badSecond(fieldJSON("PATH")), atSecond(`name "PATH"` + reserved)},
		{"reserved TZ", badSecond(fieldJSON("TZ")), atSecond(`name "TZ"` + reserved)},
		{"reserved TMPDIR", badSecond(fieldJSON("TMPDIR")), atSecond(`name "TMPDIR"` + reserved)},
		{"reserved LD_PRELOAD", badSecond(fieldJSON("LD_PRELOAD")), atSecond(`name "LD_PRELOAD"` + reserved)},
		{"reserved PLUGIN_X", badSecond(fieldJSON("PLUGIN_X")), atSecond(`name "PLUGIN_X"` + reserved)},
		{"reserved MONITOR_HUB_X", badSecond(fieldJSON("MONITOR_HUB_X")), atSecond(`name "MONITOR_HUB_X"` + reserved)},
		{"names that only look reserved", settingsDoc(fieldJSON("PATHS"), fieldJSON("TZ_NAME"), fieldJSON("LD"), fieldJSON("DATABASE_URL")), nil},
		{"duplicate name", settingsDoc(fieldJSON("WATCH_TERMS"), fieldJSON("WATCH_TERMS")), atSecond(`name "WATCH_TERMS" is declared more than once`)},
		{"blank label", badSecond(`{"name":"B","label":"  ","type":"string"}`), atSecond("label is empty")},
		{"choice without options", badSecond(`{"name":"B","label":"L","type":"choice"}`), atSecond("a choice field needs at least one option")},
		{"choice with empty options", badSecond(`{"name":"B","label":"L","type":"choice","options":[]}`), atSecond("a choice field needs at least one option")},
		{"options on a string", badSecond(`{"name":"B","label":"L","type":"string","options":["a"]}`), atSecond("options are only for choice fields")},
		{"empty options on a string", badSecond(`{"name":"B","label":"L","type":"string","options":[]}`), atSecond("options are only for choice fields")},
		{"blank option", badSecond(`{"name":"B","label":"L","type":"choice","options":["a"," "]}`), atSecond("an option is empty")},
		{"duplicate option", badSecond(`{"name":"B","label":"L","type":"choice","options":["a","b","a"]}`), atSecond(`option "a" is listed more than once`)},
		{"options differing by case", badSecond(`{"name":"B","label":"L","type":"choice","options":["a","A"]}`), nil},
		{"options differing by spaces", badSecond(`{"name":"B","label":"L","type":"choice","options":["a","a "]}`), nil},
		{"default not an option", badSecond(`{"name":"B","label":"L","type":"choice","options":["a"],"default":"z"}`), atSecond(`default "z" is not one of the options`)},
		{"default differing by case", badSecond(`{"name":"B","label":"L","type":"choice","options":["a"],"default":"A"}`), atSecond(`default "A" is not one of the options`)},
		{"secret with an empty default", badSecond(`{"name":"B","label":"L","type":"string","secret":true,"default":""}`), atSecond("a secret field cannot have a default")},
		{"not editable without reason", badSecond(`{"name":"B","label":"L","type":"string","editable":false}`), atSecond("editable: false needs a non-empty reason")},
		{"not editable with blank reason", badSecond(`{"name":"B","label":"L","type":"string","editable":false,"reason":" "}`), atSecond("editable: false needs a non-empty reason")},
		{"reason on an editable field", badSecond(`{"name":"B","label":"L","type":"string","reason":"R"}`), atSecond("reason is only for fields with editable: false")},
		{"reason on an explicitly editable field", badSecond(`{"name":"B","label":"L","type":"string","editable":true,"reason":"R"}`), atSecond("reason is only for fields with editable: false")},
		{"blank group", badSecond(`{"name":"B","label":"L","type":"string","group":" "}`), atSecond("group is empty")},
		{
			"first problem of a field in rule order",
			badSecond(`{"name":"B","label":"L","type":"string","secret":true,"default":"x","options":["a"]}`),
			atSecond("options are only for choice fields"),
		},
		{
			"two bad fields",
			settingsDoc(fieldJSON("OK_FIELD"), fieldJSON("x"), `{"name":"MODE","label":"L","type":"choice"}`),
			[]string{
				`settings.json: fields[1]: name "x" does not match ^[A-Z][A-Z0-9_]*$`,
				"settings.json: fields[2]: a choice field needs at least one option",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := settingsViolations([]byte(tt.file))
			if !slices.Equal(got, tt.want) {
				t.Fatalf("settingsViolations() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSettingsDecoderErrors(t *testing.T) {
	const prefix = "settings.json is not valid: "
	tests := []struct {
		name string
		file string
	}{
		{"wrong json type", settingsDoc(`{"name":"A","label":"L","type":"string","default":30}`)},
		{"not an object", `[]`},
		{"empty file", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := settingsViolations([]byte(tt.file))
			if len(got) != 1 || !strings.HasPrefix(got[0], prefix) || len(got[0]) == len(prefix) {
				t.Fatalf("settingsViolations() = %q, want one violation that starts with %q and has a detail", got, prefix)
			}
		})
	}
}

func TestCheckSettingsDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := map[string]string{
		"missing directory":   filepath.Join(t.TempDir(), "missing"),
		"empty path":          "",
		"file, not directory": file,
	}
	for name, dir := range unreadable {
		t.Run(name, func(t *testing.T) {
			got := CheckSettings(t.Context(), dir, "/nonexistent/plugin", nil)
			if len(got) != 1 || !strings.HasPrefix(got[0], "settings.json: cannot read: ") {
				t.Fatalf("violations = %q, want one starting with %q", got, "settings.json: cannot read: ")
			}
		})
	}
	t.Run("directory without settings.json", func(t *testing.T) {
		if got := CheckSettings(t.Context(), t.TempDir(), "/nonexistent/plugin", nil); got != nil {
			t.Fatalf("violations = %q, want none", got)
		}
	})
}
