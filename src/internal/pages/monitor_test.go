package pages

import (
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

const (
	textNoConfigure = "You do not have permission to change this monitor's configuration. An admin can give you the configure permission."
	textUnknown     = "No monitor has this name."
)

func monitorEntries(t *testing.T) []catalog.Entry {
	t.Helper()
	return []catalog.Entry{
		{Manifest: catalog.Manifest{Name: "diario"}, Settings: declarationFrom(t, "diario-settings.json")},
		{Manifest: catalog.Manifest{Name: "sei"}, Settings: declarationFrom(t, "sei-settings.json")},
	}
}

func TestTheMonitorPageShowsTheStatus(t *testing.T) {
	mux, configs, monitored, _ := newTestSite(t, Deps{Entries: monitorEntries(t)})
	configs.view = seiScene().view(declarationFrom(t, "sei-settings.json"))
	configs.start = monitorconfig.StartReport{Phase: supervisor.PhaseFailed, Reason: "connecting to the database: refused\nsecond line"}
	message := "nothing is logging in: request a check once the configuration is fixed"
	monitored.monitors = []monitors.Monitor{{Name: "sei", DisplayName: "SEI", Running: true, State: "paused", Message: message}}

	sei := serve(t.Context(), mux, http.MethodGet, "/settings/sei", "", withCookie(cookie("admin")))
	status := html.UnescapeString(inner(sei.Body.String(), "status"))
	if sei.Code != http.StatusOK {
		t.Fatalf("GET /settings/sei = %d %s, want 200", sei.Code, shown(t, sei))
	}
	for _, want := range []string{"Running", "Paused", message, `action="/settings/sei/check"`} {
		if !strings.Contains(status, want) {
			t.Errorf("the status of sei has no %q:\n%s", want, status)
		}
	}
	if !strings.Contains(sei.Body.String(), `id="editor"`) || !strings.Contains(sei.Body.String(), "<h1>SEI</h1>") {
		t.Errorf("the page of sei has no editor or no heading with its display name:\n%s", shown(t, sei))
	}

	diario := serve(t.Context(), mux, http.MethodGet, "/settings/diario", "", withCookie(cookie("admin")))
	status = html.UnescapeString(inner(diario.Body.String(), "status"))
	if diario.Code != http.StatusOK || !strings.Contains(status, "Failed to start") ||
		!strings.Contains(status, `<p class="reason pre-line">connecting to the database: refused`+"\nsecond line</p>") {
		t.Errorf("the status of diario, out of the list, = %d:\n%s\nwant 200 with Failed to start and the reason in a pre-line", diario.Code, status)
	}
	if strings.Contains(status, "Running") || strings.Contains(status, "Not running") {
		t.Errorf("the status of diario, out of the list, says whether it runs:\n%s", status)
	}

	monitored.monitors = []monitors.Monitor{{Name: "sei", State: "sleeping"}}
	stopped := serve(t.Context(), mux, http.MethodGet, "/settings/sei", "", withCookie(cookie("admin")))
	status = html.UnescapeString(inner(stopped.Body.String(), "status"))
	for _, want := range []string{"Not running", "Unknown", "Last start: Failed to start"} {
		if !strings.Contains(status, want) {
			t.Errorf("the status of sei, listed and stopped in a state the page does not know, has no %q:\n%s", want, status)
		}
	}
	monitored.monitors = []monitors.Monitor{{Name: "sei", DisplayName: "SEI", Running: true, State: "paused", Message: message}}

	configurer := serve(t.Context(), mux, http.MethodGet, "/settings/sei", "", withCookie(cookie("configurer")))
	status = html.UnescapeString(inner(configurer.Body.String(), "status"))
	if configurer.Code != http.StatusOK || !strings.Contains(status, "Last start:") {
		t.Errorf("the status of sei for sei-diario-configurer = %d:\n%s\nwant 200 with Last start:", configurer.Code, status)
	}
	for _, hidden := range []string{"/settings/sei/check", "Paused", message} {
		if strings.Contains(status, hidden) {
			t.Errorf("the status of sei for sei-diario-configurer, who may not view or check it, shows %q", hidden)
		}
	}

	configs.errs["LastStart"] = errors.Join(monitorconfig.ErrDatabase, errors.New("connection refused"))
	failed := serve(t.Context(), mux, http.MethodGet, "/settings/sei", "", withCookie(cookie("admin")))
	if failed.Code != http.StatusServiceUnavailable || !strings.Contains(text(failed), textDatabaseRead) {
		t.Errorf("GET /settings/sei with LastStart failing = %d %s, want 503 with %q", failed.Code, shown(t, failed), textDatabaseRead)
	}

	configs.errs = map[string]error{"Read": monitorconfig.ErrUnknown}
	gone := serve(t.Context(), mux, http.MethodGet, "/settings/sei", "", withCookie(cookie("admin")))
	if gone.Code != http.StatusNotFound || !strings.Contains(text(gone), textUnknown) {
		t.Errorf("GET /settings/sei with Read not knowing sei = %d %s, want 404 with %q", gone.Code, shown(t, gone), textUnknown)
	}

	configs.errs = map[string]error{}
	unknown := serve(t.Context(), mux, http.MethodGet, "/settings/nope", "", withCookie(cookie("admin")))
	if unknown.Code != http.StatusNotFound || !strings.Contains(text(unknown), textUnknown) {
		t.Errorf("GET /settings/nope = %d %s, want 404 with %q", unknown.Code, shown(t, unknown), textUnknown)
	}
}

func TestTheMonitorPageNeedsConfigure(t *testing.T) {
	mux, configs, monitored, _ := newTestSite(t, Deps{Entries: monitorEntries(t)})
	requests := map[string][]option{
		"/settings/sei":               nil,
		"/settings/sei/variables/new": {withHeader("HX-Request", "true")},
		"/settings/nope":              nil,
	}

	for target, opts := range requests {
		rec := serve(t.Context(), mux, http.MethodGet, target, "", append(opts, withCookie(cookie("viewer")))...)
		if rec.Code != http.StatusForbidden || !strings.Contains(text(rec), textNoConfigure) {
			t.Errorf("GET %s for sei-viewer = %d %s, want 403 with %q", target, rec.Code, shown(t, rec), textNoConfigure)
		}
	}
	for _, method := range []string{"Read", "Progress", "LastStart"} {
		if got := configs.callCount(method); got != 0 {
			t.Errorf("%s ran %d times for a person without configure, want 0", method, got)
		}
	}
	if got := monitored.callCount("List"); got != 0 {
		t.Errorf("List ran %d times for a person without configure, want 0", got)
	}
}

func TestTheNewVariableRow(t *testing.T) {
	mux, _, _, _ := newTestSite(t, Deps{Entries: monitorEntries(t)})

	htmx := serve(t.Context(), mux, http.MethodGet, "/settings/sei/variables/new", "", withCookie(cookie("admin")), withHeader("HX-Request", "true"))
	body := htmx.Body.String()
	if htmx.Code != http.StatusOK || strings.Contains(body, "<html") {
		t.Fatalf("htmx GET /settings/sei/variables/new = %d %s, want 200 with a fragment", htmx.Code, shown(t, htmx))
	}
	for _, name := range []string{"new_name", "new_value", "new_kind"} {
		if got := count(body, `\bname="`+name+`"`); got != 1 {
			t.Errorf("the new row has %d controls named %s, want 1", got, name)
		}
	}

	page := serve(t.Context(), mux, http.MethodGet, "/settings/sei/variables/new", "", withCookie(cookie("admin")))
	if !redirectsTo(page, "/settings/sei") {
		t.Errorf("GET /settings/sei/variables/new without htmx = %d to %q, want 303 to /settings/sei", page.Code, page.Header().Get("Location"))
	}
}

func TestMonitorTextIsEscapedAtEveryDepth(t *testing.T) {
	decl := &settings.Declaration{Fields: []settings.Field{
		{
			Name: "title", Label: "<script>alert(1)</script>", Type: settings.String, Editable: true,
			Group: "</fieldset><script>alert(5)</script>", Default: json.RawMessage(`"\" onfocus=\"alert(1)"`),
		},
		{Name: "account", Label: "Account", Type: settings.Object, Editable: true, Fields: []settings.Field{
			{Name: "user", Label: "User", Help: "<img src=x onerror=alert(1)>", Type: settings.String, Editable: true},
		}},
		{Name: "rules", Label: "Rules", Type: settings.List, Editable: true, Items: &settings.Item{Type: settings.Object, Fields: []settings.Field{
			{Name: "kind", Label: "Kind", Type: settings.Choice, Options: []string{`"><b>x</b>`}, Editable: true},
		}}},
		{Name: "cmd", Label: "Command", Type: settings.String, Reason: "<a href=javascript:alert(1)>", Env: "CMD"},
	}}
	mux, configs, monitored, people := newTestSite(t, Deps{Entries: []catalog.Entry{{Manifest: catalog.Manifest{Name: "esc"}, Settings: decl}}})
	monitored.monitors = []monitors.Monitor{{Name: "esc", DisplayName: "<img src=x onerror=alert(10)>", State: "idle", Message: "<script>alert(9)</script>"}}
	configs.start = monitorconfig.StartReport{Phase: supervisor.PhaseFailed, Reason: "<script>alert(11)</script>"}
	free := `"><script>alert(2)</script>`
	people.sessions["evil"] = access.NewSession(access.Person{Login: `x"><svg onload=alert(8)>`, Admin: true})
	view := scene{document: map[string]any{"rules": []any{map[string]any{"kind": ""}}}}.view(decl)
	view.Ignored = []monitorconfig.IgnoredView{
		{Path: "x<svg onload=alert(7)>", Value: json.RawMessage(`"<script>alert(6)</script>"`), Reason: monitorconfig.ReasonUndeclared},
	}
	view.Variables = []monitorconfig.VariableView{
		{Name: "FREE", Source: monitorconfig.SourcePage, Value: &free, Saved: true},
		{Name: `X"><svg onload=alert(4)>`, Secret: true, Source: monitorconfig.SourceEnv, EnvSet: true},
	}
	configs.view = view

	rec := serve(t.Context(), mux, http.MethodGet, "/settings/esc", "", withCookie(cookie("evil")))

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/esc = %d %s, want 200", rec.Code, shown(t, rec))
	}
	if got := count(body, `<script`); got != 1 {
		t.Errorf("the page has %d <script tags, want only the one of htmx", got)
	}
	for _, tag := range []string{"<img", "<svg", "<b>", "<a href=javascript"} {
		if strings.Contains(body, tag) {
			t.Errorf("the page has a %s tag from the monitor's or the person's text", tag)
		}
	}
	if found := handlers(body); len(found) != 0 {
		t.Errorf("the page has tags with event handlers: %q", found)
	}
	escaped := map[string]string{
		"the label of title":           "&lt;script&gt;alert(1)&lt;/script&gt;",
		"the help of account.user":     "&lt;img src=x onerror=alert(1)&gt;",
		"the group of title":           "&lt;/fieldset&gt;&lt;script&gt;alert(5)&lt;/script&gt;",
		"the reason of cmd":            "&lt;a href=javascript:alert(1)&gt;",
		"the message of the monitor":   "&lt;script&gt;alert(9)&lt;/script&gt;",
		"the display name":             "<h1>&lt;img src=x onerror=alert(10)&gt;</h1>",
		"the reason of the last start": "&lt;script&gt;alert(11)&lt;/script&gt;",
	}
	for what, want := range escaped {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %s as text", what)
		}
	}
	if got := options(body, "f-rules-0--kind"); !slices.Contains(got, choice{`"><b>x</b>`, `"><b>x</b>`, false}) {
		t.Errorf("f-rules-0--kind has the options %+v, want the declared one as text", got)
	}
	if tag := startTag(body, "var-FREE"); !hasAttr(tag, "value", free) {
		t.Errorf("var-FREE is %q, want its value as text", tag)
	}
	if pre := find(body, `<pre class="ignored-value">[^<]*</pre>`); !strings.Contains(pre, "&lt;script&gt;alert(6)&lt;/script&gt;") {
		t.Errorf("the ignored value is shown as %q, want its text escaped in the <pre>", pre)
	}
}
