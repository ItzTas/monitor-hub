package pages

import (
	"encoding/json"
	"html"
	"html/template"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

func declarationFrom(t *testing.T, file string) *settings.Declaration {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	decl, err := settings.Read(data)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return decl
}

// scene is what a View says about each field; a top-level field missing from sources is unset.
type scene struct {
	document map[string]any
	sources  map[string]monitorconfig.Source
	env      []string // top-level fields the .env sets
	saved    []string // top-level fields and secret paths saved on the page
}

func (sc scene) view(decl *settings.Declaration) monitorconfig.View {
	view := monitorconfig.View{Monitor: "sei", Declared: true, Declaration: decl, Revision: 7, Document: sc.document}
	for _, f := range decl.Fields {
		source, ok := sc.sources[f.Name]
		if !ok {
			source = monitorconfig.SourceUnset
		}
		envSet, saved := slices.Contains(sc.env, f.Name), slices.Contains(sc.saved, f.Name)
		view.Fields = append(view.Fields, monitorconfig.FieldState{Name: f.Name, Source: source, Saved: saved, EnvSet: envSet})
		if f.Secret {
			set := source == monitorconfig.SourcePage || source == monitorconfig.SourceEnv
			view.Secrets = append(view.Secrets, monitorconfig.SecretState{Path: f.Name, Set: set, Saved: saved, EnvSet: envSet})
		}
		for _, child := range f.Fields {
			path := f.Name + "." + child.Name
			if child.Secret {
				saved := slices.Contains(sc.saved, path)
				view.Secrets = append(view.Secrets, monitorconfig.SecretState{Path: path, Set: saved, Saved: saved})
			}
		}
	}
	return view
}

func renderEditor(t *testing.T, e editor) string {
	t.Helper()
	fragments, _ := parseTemplates(template.FuncMap{"asset": func(string) string { return "" }})
	var buf strings.Builder
	if err := fragments.ExecuteTemplate(&buf, "editor", e); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func seiScene() scene {
	sc := scene{
		document: map[string]any{
			"sei_url":             "https://sei.example.gov.br",
			"sei_api_url":         "https://sei.example.gov.br/api",
			"sei_username":        "paulo-santos",
			"sei_org":             "AGENCY",
			"sei_timezone":        "America/Sao_Paulo",
			"sei_units":           "all",
			"poll_interval":       "30m",
			"sei_concurrency":     "3",
			"retention_days":      "90",
			"return_warning_days": "2",
			"notification_detail": "minimal",
			"log_level":           "info",
		},
		sources: map[string]monitorconfig.Source{},
		saved:   []string{"database_url"},
	}
	for _, name := range []string{
		"sei_url", "sei_api_url", "sei_username", "sei_org", "sei_password", "sei_password_file", "sei_password_cmd",
		"sei_units", "poll_interval", "sei_concurrency", "retention_days", "return_warning_days", "notification_detail",
		"database_url", "database_url_file", "log_level",
	} {
		sc.sources[name] = monitorconfig.SourceEnv
		sc.env = append(sc.env, name)
	}
	sc.sources["sei_timezone"] = monitorconfig.SourceDefault
	sc.sources["database_url"] = monitorconfig.SourcePage
	return sc
}

// source is what the source line of the field at this id suffix says.
func source(body, suffix string) string {
	return html.UnescapeString(inner(body, "source-"+suffix))
}

func TestTheSeiFormFollowsTheDeclaration(t *testing.T) {
	view := seiScene().view(declarationFrom(t, "sei-settings.json"))

	body := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, nil, nil))

	if want := []string{"SEI account", "What to watch", "Schedule and retention", "Notifications", "Storage and logging", "Other variables"}; !slices.Equal(legends(body), want) {
		t.Errorf("the legends are %q, want %q", legends(body), want)
	}
	url := startTag(body, "f-sei_url")
	describedBy, _ := attr(url, "aria-describedby")
	if element(url) != "input" || !hasAttr(url, "type", "text") || !hasAttr(url, "name", "v:sei_url") || !hasAttr(url, "value", "https://sei.example.gov.br") {
		t.Errorf("f-sei_url is %q, want a text input named v:sei_url with its value", url)
	}
	if !slices.Contains(strings.Fields(describedBy), "help-sei_url") || !slices.Contains(strings.Fields(describedBy), "source-sei_url") {
		t.Errorf("f-sei_url is described by %q, want help-sei_url and source-sei_url", describedBy)
	}
	if !strings.Contains(html.UnescapeString(body), `<label for="f-sei_url">SEI address <span class="required">Required</span></label>`) {
		t.Error("the label of sei_url does not say it is required")
	}
	if concurrency := startTag(body, "f-sei_concurrency"); concurrency == "" || strings.Contains(concurrency, "inputmode") {
		t.Errorf("f-sei_concurrency is %q, want a text input without inputmode", concurrency)
	}
	if poll := startTag(body, "f-poll_interval"); !hasAttr(poll, "placeholder", "15m") {
		t.Errorf("f-poll_interval is %q, want the placeholder 15m", poll)
	}
	detail := options(body, "f-notification_detail")
	if element(startTag(body, "f-notification_detail")) != "select" || !slices.Equal(detail, []choice{{"minimal", "minimal", true}, {"full", "full", false}}) {
		t.Errorf("f-notification_detail has the options %v, want minimal selected and full, without an empty one", detail)
	}
	password := startTag(body, "f-sei_password")
	if value, _ := attr(password, "value"); !hasAttr(password, "type", "password") || !hasAttr(password, "name", "s:sei_password") || value != "" {
		t.Errorf("f-sei_password is %q, want an empty password input named s:sei_password", password)
	}
	if got := source(body, "sei_password"); got != "Set in .env" {
		t.Errorf("the source of sei_password says %q, want Set in .env", got)
	}
	if box(body, "clear", "database_url") == "" || box(body, "unset", "database_url") != "" {
		t.Error("the saved database_url has no clear box, or has an unset box")
	}
	for _, name := range []string{"sei_password_cmd", "sei_password_file", "database_url_file"} {
		if slices.Contains(controlNames(body), "v:"+name) || slices.Contains(controlNames(body), "s:"+name) {
			t.Errorf("%s, which is not editable, has a control", name)
		}
	}
	for _, reason := range []string{
		"Not editable here. It makes the monitor run a program inside the hub's container, so only the hub's .env sets it.",
		"Not editable here. It makes the monitor read a file inside the hub's container, so only the hub's .env sets it.",
	} {
		if !strings.Contains(html.UnescapeString(body), reason) {
			t.Errorf("the form does not say %q", reason)
		}
	}
	if got := source(body, "sei_timezone"); got != "Using the default" {
		t.Errorf("the source of sei_timezone says %q, want Using the default", got)
	}
	if !hasAttr(named(body, "revision"), "value", "7") || !hasAttr(named(body, "declaration"), "value", declarationKey(view)) {
		t.Errorf("the hidden revision and declaration are %q and %q, want 7 and the declaration key", named(body, "revision"), named(body, "declaration"))
	}

	full := seiScene()
	full.document["notification_detail"] = "FULL"
	body = renderEditor(t, buildEditor("sei", full.view(view.Declaration), monitorconfig.Progress{}, nil, nil))
	want := []choice{{"minimal", "minimal", false}, {"full", "full", false}, {"FULL", "FULL (not one of the options)", true}}
	if got := options(body, "f-notification_detail"); !slices.Equal(got, want) {
		t.Errorf("with FULL in the .env, f-notification_detail has %v, want %v", got, want)
	}

	unset := seiScene()
	delete(unset.document, "notification_detail")
	body = renderEditor(t, buildEditor("sei", unset.view(view.Declaration), monitorconfig.Progress{}, nil, nil))
	if got := options(body, "f-notification_detail"); len(got) == 0 || got[0] != (choice{"", "Default (minimal)", true}) {
		t.Errorf("without a value, f-notification_detail has %v, want Default (minimal) first and selected", got)
	}
}

func TestTheDiarioFormHasAListOfTerms(t *testing.T) {
	sc := scene{
		document: map[string]any{"watch_terms": []any{"PAULO SANTOS", "0034712k"}, "check_interval_minutes": "30"},
		sources: map[string]monitorconfig.Source{
			"watch_terms": monitorconfig.SourceEnv, "check_interval_minutes": monitorconfig.SourceDefault,
			"database_url": monitorconfig.SourceEnv, "gazette_api_url": monitorconfig.SourceDefault,
		},
		env: []string{"watch_terms", "database_url"},
	}

	body := renderEditor(t, buildEditor("diario", sc.view(declarationFrom(t, "diario-settings.json")), monitorconfig.Progress{}, nil, nil))

	for id, want := range map[string]string{"f-watch_terms-0": "PAULO SANTOS", "f-watch_terms-1": "0034712k"} {
		if tag := startTag(body, id); !hasAttr(tag, "value", want) {
			t.Errorf("%s is %q, want the value %q", id, tag, want)
		}
	}
	if count := named(body, "n:watch_terms"); !hasAttr(count, "value", "2") {
		t.Errorf("n:watch_terms is %q, want 2", count)
	}
	for _, value := range []string{"remove:watch_terms[0]", "remove:watch_terms[1]", "add:watch_terms"} {
		b := button(body, value)
		if !hasAttr(b, "formaction", "/settings/diario/form") || !hasAttr(b, "hx-post", "/settings/diario/form") {
			t.Errorf("the button %s is %q, want formaction and hx-post /settings/diario/form", value, b)
		}
	}
	if slices.Contains(controlNames(body), "v:gazette_api_url") || startTag(body, "source-gazette_api_url") == "" {
		t.Error("gazette_api_url has a control or no source")
	}
	if !strings.Contains(html.UnescapeString(body), "Not editable here. Only the tests change it.") {
		t.Error("the form does not say why gazette_api_url is not editable")
	}
	if !hasAttr(startTag(body, "f-database_url"), "type", "password") {
		t.Errorf("f-database_url is %q, want a password input", startTag(body, "f-database_url"))
	}
}

func nestedScene() scene {
	return scene{
		document: map[string]any{
			"account": map[string]any{"user": "paulo-santos"},
			"rules": []any{
				map[string]any{"process": "a", "actions": []any{"urgent"}, "limit": json.Number("3"), "notify": true},
				map[string]any{"process": "b", "actions": []any{"ignore", "urgent"}},
			},
			"ratio":   json.Number("1.5"),
			"enabled": true,
			"when":    "15m",
			"level":   "low",
			"count":   json.Number("2"),
		},
		sources: map[string]monitorconfig.Source{
			"account": monitorconfig.SourcePage, "rules": monitorconfig.SourcePage, "ratio": monitorconfig.SourceEnv,
			"enabled": monitorconfig.SourceDefault, "when": monitorconfig.SourceDefault, "level": monitorconfig.SourceEnv,
			"count": monitorconfig.SourceEnv, "secret_key": monitorconfig.SourceEnv,
		},
		env:   []string{"ratio", "level", "count", "secret_key"},
		saved: []string{"account", "rules", "when", "account.token"},
	}
}

func TestNestedDeclarationsRender(t *testing.T) {
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))

	body := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, nil, nil))

	account := fieldset(body, "Account")
	if startTag(account, "f-account--user") == "" || !hasAttr(startTag(account, "f-account--token"), "name", "s:account.token") {
		t.Errorf("the Account fieldset does not hold f-account--user and f-account--token named s:account.token:\n%s", account)
	}
	for _, legend := range []string{"Item 1", "Item 2"} {
		if got := count(strings.Join(legends(body), "\n"), "(?m)^"+legend+"$"); got != 1 {
			t.Errorf("%d legends read %s, want 1", got, legend)
		}
	}
	for value, want := range map[string]bool{"up:rules[0]": false, "down:rules[0]": true, "up:rules[1]": true, "down:rules[1]": false, "remove:rules[1]": true} {
		if got := button(body, value) != ""; got != want {
			t.Errorf("the button %s is there: %v, want %v", value, got, want)
		}
	}
	actions := options(body, "f-rules-0--actions-0")
	if element(startTag(body, "f-rules-0--actions-0")) != "select" || !slices.Equal(actions, []choice{{"urgent", "urgent", true}, {"ignore", "ignore", false}}) {
		t.Errorf("f-rules-0--actions-0 has %v, want urgent selected and ignore", actions)
	}
	if limit := startTag(body, "f-rules-0--limit"); !hasAttr(limit, "inputmode", "numeric") || !hasAttr(limit, "value", "3") {
		t.Errorf("f-rules-0--limit is %q, want inputmode numeric and the value 3", limit)
	}
	if notify := options(body, "f-rules-0--notify"); !slices.Equal(notify, []choice{{"true", "Yes", true}, {"false", "No", false}}) {
		t.Errorf("f-rules-0--notify has %v, want Yes selected and No", notify)
	}
	if notify := options(body, "f-rules-1--notify"); !slices.Equal(notify, []choice{{"", "Not set", true}, {"true", "Yes", false}, {"false", "No", false}}) {
		t.Errorf("f-rules-1--notify without a value has %v, want Not set selected, Yes and No", notify)
	}
	if ratio := startTag(body, "f-ratio"); !hasAttr(ratio, "inputmode", "decimal") || !hasAttr(ratio, "value", "1.5") {
		t.Errorf("f-ratio is %q, want inputmode decimal and the value 1.5", ratio)
	}
	if enabled := options(body, "f-enabled"); !slices.Equal(enabled, []choice{{"true", "Yes", true}, {"false", "No", false}}) {
		t.Errorf("f-enabled has %v, want Yes selected and No, without an empty one", enabled)
	}
	labels := html.UnescapeString(fieldset(body, "Labels"))
	if !strings.Contains(labels, "No items yet.") || !strings.Contains(labels, `Add<span class="visually-hidden"> an item to Labels</span>`) {
		t.Errorf("the empty Labels list does not say it has no items and offer to add one:\n%s", labels)
	}
	for name, want := range map[string]string{"n:rules": "2", "n:rules[0].actions": "1", "n:rules[1].actions": "2", "n:labels": "0"} {
		if tag := named(body, name); !hasAttr(tag, "value", want) {
			t.Errorf("%s is %q, want %s", name, tag, want)
		}
	}
	if slices.Contains(controlNames(body), "s:secret_key") || slices.Contains(controlNames(body), "v:secret_key") || startTag(body, "f-secret_key") != "" {
		t.Error("secret_key, which is not editable, has a control")
	}
	if first := find(body, `<button\s[^>]*>`); !hasAttr(first, "type", "submit") || strings.Contains(first, "name=") {
		t.Errorf("the first button of the form is %q, want one that saves, since Enter in a field presses it", first)
	}
	reader := html.UnescapeString(body)
	for _, want := range []string{`name="unset" value="rules"> Clear</label>`, `name="unset" value="when"> Use the default</label>`, `name="clear" value="account.token"> Clear</label>`} {
		if !strings.Contains(reader, want) {
			t.Errorf("the form has no box %s", want)
		}
	}
	if !hasAttr(startTag(body, "f-count"), "placeholder", "A whole number") || !strings.Contains(reader, `<p id="help-when" class="help">Default when empty: 15m</p>`) ||
		!strings.Contains(reader, `<p id="help-enabled" class="help">Default when empty: Yes</p>`) {
		t.Error("f-count has no example, or when and enabled do not say their defaults")
	}
	if got := source(body, "account--token"); got != "Set on this page" {
		t.Errorf("the source of account.token says %q, want Set on this page", got)
	}
}

func TestIgnoredValuesAndEnvProblemsAreShown(t *testing.T) {
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))
	view.Ignored = []monitorconfig.IgnoredView{
		{Path: "old_name", Value: json.RawMessage(`{"a":[1,2]}`), Reason: monitorconfig.ReasonUndeclared},
		{Path: "old_token", Secret: true, Reason: monitorconfig.ReasonUndeclared},
		{Path: "ratio", Value: json.RawMessage(`"x"`), Reason: monitorconfig.ReasonType},
		{Path: "secret_key", Secret: true, Reason: monitorconfig.ReasonNotEditable},
		{Path: "older", Value: json.RawMessage(`1`), Reason: monitorconfig.ReasonNoDeclaration},
		{Path: "account.token", Secret: true, Reason: monitorconfig.ReasonReplaced},
	}
	view.Problems = []monitorconfig.Problem{{Path: "count", Message: `COUNT is not an integer ("three")`}}

	body := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, nil, nil))

	rows := map[string]string{}
	for i, ignored := range view.Ignored {
		rows[ignored.Path] = html.UnescapeString(inner(body, "ignored-"+strconv.Itoa(i)))
	}
	if want := "<pre class=\"ignored-value\">{\n  \"a\": [\n    1,\n    2\n  ]\n}</pre>"; !strings.Contains(rows["old_name"], want) {
		t.Errorf("the row of old_name does not show its value indented:\n%s", rows["old_name"])
	}
	texts := map[string][]string{
		"old_name":      {"The monitor no longer declares this field."},
		"old_token":     {"The monitor no longer declares this field.", "Secret value. The page does not show it."},
		"ratio":         {"The saved value no longer fits this field."},
		"secret_key":    {"The monitor does not allow changing this field here."},
		"older":         {"The monitor has no settings.json that this hub can read."},
		"account.token": {"This value was saved before the field became secret. The secret saved later takes its place.", "Secret value. The page does not show it."},
	}
	for path, want := range texts {
		for _, w := range want {
			if !strings.Contains(rows[path], w) {
				t.Errorf("the row of %s does not say %q:\n%s", path, w, rows[path])
			}
		}
		if box(body, "drop", path) == "" {
			t.Errorf("the row of %s has no drop box", path)
		}
	}
	for _, path := range []string{"old_token", "secret_key", "account.token"} {
		if strings.Contains(rows[path], "<pre") {
			t.Errorf("the row of the secret %s shows a value:\n%s", path, rows[path])
		}
	}
	reader := html.UnescapeString(body)
	if !strings.Contains(reader, "The monitor cannot start because these values in the hub's .env file do not fit their fields. Set them on this page, or fix the .env file and restart the hub:") ||
		!strings.Contains(reader, `count: COUNT is not an integer ("three")`) {
		t.Error("the form does not list the problems of the .env")
	}
}

func TestOtherVariablesAndUndeclaredMonitors(t *testing.T) {
	free, typed := "v", ""
	view := seiScene().view(declarationFrom(t, "sei-settings.json"))
	view.Variables = []monitorconfig.VariableView{
		{Name: "FREE", Source: monitorconfig.SourcePage, Value: &free, Saved: true},
		{Name: "PROXY", Secret: true, Source: monitorconfig.SourceEnv, EnvSet: true},
		{Name: "TOKEN", Secret: true, Source: monitorconfig.SourcePage, Saved: true},
		{Name: "EMPTY", Source: monitorconfig.SourcePage, Value: &typed, Saved: true, EnvSet: true},
	}
	view.Ignored = []monitorconfig.IgnoredView{
		{Path: "cmd", Value: json.RawMessage(`"x"`), Reason: monitorconfig.ReasonNotEditable},
		{Path: "old_password", Secret: true, Reason: monitorconfig.ReasonReplaced},
	}

	body := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, nil, nil))
	reader := html.UnescapeString(body)

	if tag := startTag(body, "var-FREE"); !hasAttr(tag, "type", "text") || !hasAttr(tag, "name", "var.FREE") || !hasAttr(tag, "value", "v") {
		t.Errorf("var-FREE is %q, want a text input named var.FREE with v", tag)
	}
	if !strings.Contains(reader, `value="FREE"> Remove</label>`) || !strings.Contains(reader, `value="EMPTY"> Use the .env value</label>`) {
		t.Error("the saved variables have no Remove and Use the .env value boxes")
	}
	if got := source(body, "var-EMPTY"); got != "Saved empty on this page" {
		t.Errorf("the source of EMPTY says %q, want Saved empty on this page", got)
	}
	if !hasAttr(startTag(body, "var-PROXY"), "type", "password") || source(body, "var-PROXY") != "Set in .env. The hub does not show the value because it cannot tell whether it is a secret." {
		t.Errorf("var-PROXY is %q with the source %q, want a password input and the .env text", startTag(body, "var-PROXY"), source(body, "var-PROXY"))
	}
	token := startTag(body, "var-TOKEN")
	if value, _ := attr(token, "value"); !hasAttr(token, "type", "password") || value != "" {
		t.Errorf("var-TOKEN is %q, want an empty password input", token)
	}
	for _, id := range []string{"new-0-name", "new-0-value", "new-0-kind"} {
		if startTag(body, id) == "" {
			t.Errorf("the blank new row has no %s", id)
		}
	}
	add := find(body, `<button\s[^>]*\bhx-get="/settings/sei/variables/new"[^>]*>`)
	if class, _ := attr(add, "class"); !slices.Contains(strings.Fields(class), "needs-js") {
		t.Errorf("the button that adds a variable is %q, want the class needs-js", add)
	}
	for path, want := range map[string]string{"cmd": "The monitor does not allow changing this field here.", "old_password": "This value was saved before the field became secret. The secret saved later takes its place."} {
		if !strings.Contains(reader, want) || box(body, "drop", path) == "" {
			t.Errorf("the ignored %s has no reason or no drop box", path)
		}
	}

	bare := monitorconfig.View{Monitor: "anhanguera", Revision: 3, Ignored: []monitorconfig.IgnoredView{
		{Path: "x", Value: json.RawMessage(`"1"`), Reason: monitorconfig.ReasonNoDeclaration},
	}}
	body = renderEditor(t, buildEditor("anhanguera", bare, monitorconfig.Progress{}, nil, nil))
	reader = html.UnescapeString(body)
	if !strings.Contains(reader, "This monitor does not declare its settings, so nothing can be changed here. Its values come from the hub's .env file.") {
		t.Error("the form of a monitor without a declaration does not say so")
	}
	if got := controlNames(body); !slices.Equal(got, []string{"revision", "declaration", "drop"}) {
		t.Errorf("the form of a monitor without a declaration has the controls %q, want revision, declaration and drop", got)
	}
	if !strings.Contains(reader, `<button type="submit">Delete the selected values</button>`) || box(body, "drop", "x") == "" {
		t.Error("the form of a monitor without a declaration has no drop box for x or no button to delete it")
	}
}

func TestTheFormEchoesWhatWasSent(t *testing.T) {
	free := "v"
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))
	view.Variables = []monitorconfig.VariableView{
		{Name: "FREE", Source: monitorconfig.SourcePage, Value: &free, Saved: true},
		{Name: "PROXY", Secret: true, Source: monitorconfig.SourceEnv, EnvSet: true},
	}
	posted := &submission{
		Texts: map[string]string{
			"ratio": "1.50x", "rules[0].process": "p0", "rules[0].actions[0]": "ignore", "rules[1].process": "p1",
			"labels[0]": "a", "level": "loud",
		},
		Counts:    map[string]int{"rules": 2, "rules[0].actions": 1, "rules[1].actions": 0, "labels": 1},
		Secrets:   map[string]string{"account.token": "typed-secret-one"},
		Unset:     map[string]bool{"rules": true},
		Clear:     map[string]bool{"account.token": true},
		Variables: map[string]string{"FREE": "w", "PROXY": "typed-secret-two"},
		VarUnset:  map[string]bool{"FREE": true},
		New:       []newRow{{Name: "HTTP_PROXY", Value: "http://p"}, {Name: "API_KEY", Value: "typed-secret-three", Secret: true}},
	}
	view.Ignored = []monitorconfig.IgnoredView{{Path: "old_name", Value: json.RawMessage(`1`), Reason: monitorconfig.ReasonUndeclared}}
	posted.Drop = map[string]bool{"old_name": true}
	problems := []monitorconfig.Problem{
		{Path: "ratio", Message: "use a number"}, {Path: "", Message: "set exactly one password"}, {Path: "nope", Message: "x"},
		{Path: "env:FREE", Message: "m2"}, {Path: "ignored:old_name", Message: "m3"},
	}

	body := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, posted, problems))
	reader := html.UnescapeString(body)

	ratio := startTag(body, "f-ratio")
	describedBy, _ := attr(ratio, "aria-describedby")
	if !hasAttr(ratio, "value", "1.50x") || !hasAttr(ratio, "aria-invalid", "true") || !slices.Contains(strings.Fields(describedBy), "error-ratio") {
		t.Errorf("f-ratio is %q, want the sent 1.50x, aria-invalid and error-ratio", ratio)
	}
	if got := html.UnescapeString(inner(body, "error-ratio")); !strings.HasSuffix(got, "use a number") {
		t.Errorf("error-ratio says %q, want use a number", got)
	}
	summary := html.UnescapeString(inner(body, "error-summary"))
	for _, want := range []string{`href="#f-ratio"`, "set exactly one password", "nope: x", `href="#var-FREE"`, `href="#ignored-0"`} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary has no %q:\n%s", want, summary)
		}
	}
	if tag := startTag(body, "error-summary"); !hasAttr(tag, "role", "alert") || !hasAttr(tag, "tabindex", "-1") {
		t.Errorf("the summary is %q, want role alert and tabindex -1", tag)
	}
	if !strings.Contains(reader, "Secret values are never sent back to the page. Type them again.") {
		t.Error("the form does not say the typed secrets must be typed again")
	}
	for label, secret := range map[string]string{"one": "typed-secret-one", "two": "typed-secret-two", "three": "typed-secret-three"} {
		if strings.Contains(body, secret) {
			t.Errorf("the form carries the typed secret %s", label)
		}
	}
	for id, want := range map[string]string{"f-rules-0--process": "p0", "f-rules-1--process": "p1", "f-labels-0": "a", "var-FREE": "w", "new-0-value": "http://p", "new-0-name": "HTTP_PROXY", "new-1-name": "API_KEY"} {
		if !hasAttr(startTag(body, id), "value", want) {
			t.Errorf("%s is %q, want the sent %q", id, startTag(body, id), want)
		}
	}
	if value, _ := attr(startTag(body, "new-1-value"), "value"); value != "" || !slices.Contains(options(body, "new-1-kind"), choice{"secret", "Secret", true}) {
		t.Errorf("the secret new row is %q with %v, want no value and Secret selected", startTag(body, "new-1-value"), options(body, "new-1-kind"))
	}
	if got := options(body, "f-level"); !slices.Contains(got, choice{"loud", "loud (not one of the options)", true}) {
		t.Errorf("f-level has %v, want the sent loud selected", got)
	}
	for name, want := range map[string]string{"n:rules": "2", "n:rules[1].actions": "0", "n:labels": "1"} {
		if !hasAttr(named(body, name), "value", want) {
			t.Errorf("%s is %q, want %s", name, named(body, name), want)
		}
	}
	if got := html.UnescapeString(inner(body, "error-var-FREE")); !strings.HasSuffix(got, "m2") || !strings.Contains(html.UnescapeString(inner(body, "ignored-0")), "m3") {
		t.Errorf("the problems of FREE and old_name are not in their rows: %q", got)
	}
	for _, b := range [][2]string{{"unset", "rules"}, {"clear", "account.token"}, {"var_unset", "FREE"}, {"drop", "old_name"}} {
		if _, checked := attr(box(body, b[0], b[1]), "checked"); !checked {
			t.Errorf("the %s box of %s is not checked as sent", b[0], b[1])
		}
	}
}

func TestASecretVariableTypedAgainIsNotEchoed(t *testing.T) {
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))
	view.Variables = []monitorconfig.VariableView{{Name: "PROXY", Secret: true, Source: monitorconfig.SourceEnv, EnvSet: true}}
	sent := map[string]*submission{
		"in its own row": {Variables: map[string]string{"PROXY": "typed-secret-one"}},
		"in a new row":   {New: []newRow{{Name: "PROXY", Value: "typed-secret-two"}}},
	}

	for where, posted := range sent {
		body := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, posted, nil))
		if !strings.Contains(html.UnescapeString(body), "Secret values are never sent back to the page. Type them again.") {
			t.Errorf("with PROXY typed %s, the form does not say the typed secrets must be typed again", where)
		}
		if strings.Contains(body, "typed-secret") {
			t.Errorf("with PROXY typed %s, the form carries the typed value", where)
		}
	}
	body := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{}, sent["in a new row"], nil))
	if !hasAttr(startTag(body, "new-0-name"), "value", "PROXY") {
		t.Errorf("the new row is %q, want its name PROXY back", startTag(body, "new-0-name"))
	}
}

func TestARestartHoldsTheSaveButton(t *testing.T) {
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))

	body := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{State: monitorconfig.ProgressRestarting}, nil, nil))

	poll := find(body, `<div\s[^>]*\bhx-get="/settings/sei/progress\?shown=restarting"[^>]*>`)
	if !hasAttr(poll, "hx-trigger", "every 1s") || !hasAttr(poll, "hx-target", "#outcome") {
		t.Errorf("the outcome polls with %q, want every 1s into #outcome", poll)
	}
	if !strings.Contains(html.UnescapeString(inner(body, "outcome")), "Saved. The monitor is restarting.") {
		t.Error("the outcome does not say the monitor is restarting")
	}
	if save := startTag(body, "save"); !strings.Contains(save, " disabled") || !strings.Contains(body, ">Restarting</button>") {
		t.Errorf("the save button is %q, want it disabled and saying Restarting", save)
	}

	idle := renderEditor(t, buildEditor("sei", view, monitorconfig.Progress{State: monitorconfig.ProgressNone}, nil, nil))
	if strings.Contains(idle, "hx-trigger") || strings.Contains(startTag(idle, "save"), "disabled") || !strings.Contains(idle, ">Save and restart</button>") {
		t.Error("with no restart going, the form polls or holds the save button")
	}
}
