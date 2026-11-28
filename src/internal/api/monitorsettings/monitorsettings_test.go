package monitorsettings_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/accounts/accountstest"
	"github.com/ItzTas/monitor-hub/src/internal/api"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitorsettings"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

const (
	secretValue = "row-db-secret-value"
	settingsURL = "/api/monitors/sei/settings"
	progressURL = "/api/monitors/sei/settings/progress"
)

type fakeService struct {
	mu       sync.Mutex
	view     monitorconfig.View
	result   monitorconfig.SaveResult
	progress monitorconfig.Progress
	err      error
	changes  []monitorconfig.Change
	calls    map[string]int
}

func (f *fakeService) note(method string) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
}

func (f *fakeService) Read(context.Context, string) (monitorconfig.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("Read")
	return f.view, f.err
}

func (f *fakeService) Save(_ context.Context, _ string, change monitorconfig.Change) (monitorconfig.SaveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("Save")
	f.changes = append(f.changes, change)
	return f.result, f.err
}

func (f *fakeService) Progress(context.Context, string) (monitorconfig.Progress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("Progress")
	return f.progress, f.err
}

func (f *fakeService) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, count := range f.calls {
		n += count
	}
	return n
}

var permissions = map[string][]string{
	"sei-configurer":    {"monitor:sei:configure"},
	"all-configurer":    {"monitor:*:configure"},
	"sei-viewer":        {"monitor:sei:view", "monitor:sei:check"},
	"diario-configurer": {"monitor:diario:configure"},
}

type bench struct {
	mux  *http.ServeMux
	svc  *accounts.Service
	fake *fakeService
	logs *bytes.Buffer
}

func newBench(t *testing.T) *bench {
	t.Helper()
	b := &bench{
		mux:  http.NewServeMux(),
		svc:  accounts.NewService(coretest.Pool(t), time.Now),
		fake: &fakeService{},
		logs: &bytes.Buffer{},
	}
	accountstest.Admin(t, b.svc, "paulo-santos")
	humaAPI := humago.New(b.mux, api.HumaConfig(slog.New(slog.NewTextHandler(b.logs, nil)), "test", "test"))
	humaAPI.UseMiddleware(gate.Middleware(humaAPI, b.svc, ""))
	monitorsettings.RegisterRoutes(humaAPI, monitorsettings.NewHandler(b.fake))
	t.Cleanup(func() {
		if logs := b.logs.String(); redacted(logs) != logs {
			t.Error("the logs carry a password or a secret value")
		}
	})
	return b
}

type edit func(*http.Request)

// session signs login in, creating the person first unless it is paulo-santos.
func (b *bench) session(t *testing.T, login string) edit {
	t.Helper()
	if login != "paulo-santos" {
		accountstest.Person(t, b.svc, login, permissions[login]...)
	}
	id := accountstest.Session(t, b.svc, login)
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: gate.CookieName, Value: id}) }
}

func withBearer(token string) edit {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func withOrigin(origin string) edit {
	return func(r *http.Request) { r.Header.Set("Origin", origin) }
}

func withoutOrigin(r *http.Request) {
	r.Header.Del("Origin")
}

func (b *bench) serve(t *testing.T, method, target, body string, edits ...edit) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	request.Header.Set("Origin", "http://example.com")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, e := range edits {
		e(request)
	}
	rec := httptest.NewRecorder()
	b.mux.ServeHTTP(rec, request)
	return rec
}

// shown is the body of rec for a failure message. A password or secret value in it is an
// error of its own, and the copy shown has it replaced.
func shown(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := rec.Body.String()
	if redacted(body) != body {
		t.Error("a response body carries a password or a secret value")
	}
	return redacted(body)
}

func redacted(text string) string {
	return strings.NewReplacer(accountstest.Password, "[password]", secretValue, "[secret]").Replace(text)
}

func detail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var model struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &model); err != nil {
		t.Fatalf("decode %s: %v", shown(t, rec), err)
	}
	return model.Detail
}

func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("decode %s: %v", redacted(got), err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(g, w)
}

type call struct {
	method, target, body string
}

var routes = []call{
	{http.MethodGet, settingsURL, ""},
	{http.MethodPut, settingsURL, `{"document":{"x":1}}`},
	{http.MethodGet, progressURL, ""},
}

func TestConfigurationNeedsTheConfigurePermission(t *testing.T) {
	for _, login := range []string{"paulo-santos", "sei-configurer", "all-configurer"} {
		t.Run(login, func(t *testing.T) {
			b := newBench(t)
			b.fake.result = monitorconfig.SaveResult{Changed: []string{"x"}, Restarted: true, Request: 1}
			b.fake.progress = monitorconfig.Progress{State: monitorconfig.ProgressNone, Phase: supervisor.PhaseNotStarted}
			session := b.session(t, login)
			for i, want := range []int{http.StatusOK, http.StatusAccepted, http.StatusOK} {
				r := routes[i]
				if rec := b.serve(t, r.method, r.target, r.body, session); rec.Code != want {
					t.Errorf("%s %s = %d %s, want %d", r.method, r.target, rec.Code, shown(t, rec), want)
				}
			}
		})
	}

	refused := []struct {
		name   string
		caller func(*testing.T, *bench) edit
		status int
		detail string
	}{
		{"sei-viewer", func(t *testing.T, b *bench) edit { return b.session(t, "sei-viewer") }, http.StatusForbidden, "you do not have permission for this"},
		{"diario-configurer", func(t *testing.T, b *bench) edit { return b.session(t, "diario-configurer") }, http.StatusForbidden, "you do not have permission for this"},
		{"a token", func(t *testing.T, b *bench) edit {
			return withBearer(accountstest.Token(t, b.svc, "paulo-santos", "monitor:*:view"))
		}, http.StatusForbidden, "this route needs a signed-in session"},
		{"nobody", func(*testing.T, *bench) edit { return func(*http.Request) {} }, http.StatusUnauthorized, "sign in or send a personal token"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			caller := tc.caller(t, b)
			for _, r := range routes {
				rec := b.serve(t, r.method, r.target, r.body, caller)
				if rec.Code != tc.status || detail(t, rec) != tc.detail {
					t.Errorf("%s %s = %d %s, want %d %q", r.method, r.target, rec.Code, shown(t, rec), tc.status, tc.detail)
				}
				if tc.status == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Errorf("%s %s has WWW-Authenticate %q, want Bearer", r.method, r.target, rec.Header().Get("WWW-Authenticate"))
				}
			}
			if n := b.fake.total(); n != 0 {
				t.Errorf("the service was called %d times, want none", n)
			}
		})
	}
}

func TestARefusalDoesNotTellWhetherTheMonitorExists(t *testing.T) {
	b := newBench(t)
	b.fake.err = monitorconfig.ErrUnknown

	refused := b.serve(t, http.MethodGet, "/api/monitors/nope/settings", "", b.session(t, "sei-configurer"))
	if refused.Code != http.StatusForbidden || b.fake.total() != 0 {
		t.Errorf("sei-configurer on nope = %d %s with %d calls, want 403 and none", refused.Code, shown(t, refused), b.fake.total())
	}

	unknown := b.serve(t, http.MethodGet, "/api/monitors/nope/settings", "", b.session(t, "paulo-santos"))
	if unknown.Code != http.StatusNotFound || detail(t, unknown) != "no monitor has this name" {
		t.Errorf("an admin on nope = %d %s, want 404 no monitor has this name", unknown.Code, shown(t, unknown))
	}
}

func TestSavingNeedsTheHubsOrigin(t *testing.T) {
	b := newBench(t)
	b.fake.result = monitorconfig.SaveResult{Changed: []string{"x"}, Restarted: true, Request: 1}
	session := b.session(t, "paulo-santos")
	body := `{"document":{"x":1}}`

	for _, tc := range []struct {
		name   string
		origin edit
	}{{"no Origin", withoutOrigin}, {"another site", withOrigin("http://evil.example")}} {
		rec := b.serve(t, http.MethodPut, settingsURL, body, session, tc.origin)
		if rec.Code != http.StatusForbidden || detail(t, rec) != "the request comes from another site" {
			t.Errorf("PUT with %s = %d %s, want 403 the request comes from another site", tc.name, rec.Code, shown(t, rec))
		}
	}
	if rec := b.serve(t, http.MethodPut, settingsURL, body, session); rec.Code != http.StatusAccepted {
		t.Errorf("PUT from the hub = %d %s, want 202", rec.Code, shown(t, rec))
	}
	if n := b.fake.calls["Save"]; n != 1 {
		t.Errorf("Save ran %d times, want 1", n)
	}
}

func text(s string) *string {
	return &s
}

func declaredView() monitorconfig.View {
	return monitorconfig.View{
		Monitor:  "sei",
		Declared: true,
		Revision: 3,
		Declaration: &settings.Declaration{Fields: []settings.Field{
			{Name: "url", Label: "SEI address", Help: "Without /sei at the end.", Type: settings.String, Required: true, Editable: true, Group: "SEI", Env: "SEI_URL"},
			{Name: "password", Label: "Password", Type: settings.String, Required: true, Secret: true, Editable: true, Env: "SEI_PASSWORD"},
			{Name: "mode", Label: "Mode", Type: settings.Choice, Editable: true, Options: []string{"full", "summary"}, Default: json.RawMessage(`"full"`)},
			{Name: "mail", Label: "Mail", Type: settings.Object, Editable: true, Fields: []settings.Field{
				{Name: "host", Label: "Host", Type: settings.String, Editable: true},
				{Name: "password", Label: "Mail password", Type: settings.String, Secret: true, Editable: true},
			}},
			{Name: "tags", Label: "Tags", Type: settings.List, Editable: true, Items: &settings.Item{Type: settings.Object, Fields: []settings.Field{
				{Name: "kind", Label: "Kind", Type: settings.Choice, Editable: true, Options: []string{"a", "b"}},
			}}},
			{Name: "interval", Label: "Interval", Type: settings.Duration, Reason: "The hub sets it.", Default: json.RawMessage(`"10m"`)},
		}},
		Document: map[string]any{
			"url":  "https://sei.example.invalid",
			"mode": "full",
			"mail": map[string]any{"host": "smtp.example.invalid"},
			"tags": []any{map[string]any{"kind": "a"}},
		},
		Fields: []monitorconfig.FieldState{
			{Name: "url", Source: monitorconfig.SourcePage, Saved: true, EnvSet: true},
			{Name: "password", Source: monitorconfig.SourceEnv, EnvSet: true},
			{Name: "mode", Source: monitorconfig.SourceDefault},
			{Name: "mail", Source: monitorconfig.SourcePage, Saved: true},
			{Name: "tags", Source: monitorconfig.SourcePage, Saved: true},
			{Name: "interval", Source: monitorconfig.SourceDefault},
		},
		Secrets: []monitorconfig.SecretState{
			{Path: "password", Set: true, EnvSet: true},
			{Path: "mail.password", Set: true, Saved: true},
		},
		Ignored: []monitorconfig.IgnoredView{
			{Path: "old", Value: json.RawMessage(`{"a":1}`), Reason: monitorconfig.ReasonUndeclared},
			{Path: "token", Secret: true, Reason: monitorconfig.ReasonType},
			{Path: "password", Secret: true, Reason: monitorconfig.ReasonReplaced},
		},
		Variables: []monitorconfig.VariableView{
			{Name: "EXTRA", Source: monitorconfig.SourcePage, Value: text("on"), Saved: true},
			{Name: "TOKEN", Secret: true, Source: monitorconfig.SourceEnv, EnvSet: true},
		},
		Problems: []monitorconfig.Problem{{Path: "mode", Message: "the default is not one of the options"}},
	}
}

const declaredJSON = `{"monitor":"sei","declared":true,"revision":3,
"declaration":{"fields":[
 {"name":"url","label":"SEI address","help":"Without /sei at the end.","type":"string","required":true,"secret":false,"editable":true,"group":"SEI","env":"SEI_URL"},
 {"name":"password","label":"Password","type":"string","required":true,"secret":true,"editable":true,"env":"SEI_PASSWORD"},
 {"name":"mode","label":"Mode","type":"choice","required":false,"secret":false,"editable":true,"options":["full","summary"],"default":"full"},
 {"name":"mail","label":"Mail","type":"object","required":false,"secret":false,"editable":true,"fields":[
   {"name":"host","label":"Host","type":"string","required":false,"secret":false,"editable":true},
   {"name":"password","label":"Mail password","type":"string","required":false,"secret":true,"editable":true}]},
 {"name":"tags","label":"Tags","type":"list","required":false,"secret":false,"editable":true,"items":{"type":"object","fields":[
   {"name":"kind","label":"Kind","type":"choice","required":false,"secret":false,"editable":true,"options":["a","b"]}]}},
 {"name":"interval","label":"Interval","type":"duration","required":false,"secret":false,"editable":false,"reason":"The hub sets it.","default":"10m"}]},
"document":{"url":"https://sei.example.invalid","mode":"full","mail":{"host":"smtp.example.invalid"},"tags":[{"kind":"a"}]},
"fields":[
 {"name":"url","source":"page","saved":true,"env_set":true},
 {"name":"password","source":"env","saved":false,"env_set":true},
 {"name":"mode","source":"default","saved":false,"env_set":false},
 {"name":"mail","source":"page","saved":true,"env_set":false},
 {"name":"tags","source":"page","saved":true,"env_set":false},
 {"name":"interval","source":"default","saved":false,"env_set":false}],
"secrets":[
 {"path":"password","set":true,"saved":false,"env_set":true},
 {"path":"mail.password","set":true,"saved":true,"env_set":false}],
"ignored":[
 {"path":"old","value":{"a":1},"secret":false,"reason":"undeclared"},
 {"path":"token","value":null,"secret":true,"reason":"type"},
 {"path":"password","value":null,"secret":true,"reason":"replaced"}],
"variables":[
 {"name":"EXTRA","secret":false,"source":"page","value":"on","saved":true,"env_set":false},
 {"name":"TOKEN","secret":true,"source":"env","value":null,"saved":false,"env_set":true}],
"problems":[{"path":"mode","message":"the default is not one of the options"}]}`

func TestSettingsReadShowsTheView(t *testing.T) {
	cases := []struct {
		name string
		view monitorconfig.View
		want string
	}{
		{"a declared monitor", declaredView(), declaredJSON},
		{"a monitor without declaration", monitorconfig.View{
			Monitor:  "bare",
			Revision: 2,
			Ignored:  []monitorconfig.IgnoredView{{Path: "n", Value: json.RawMessage(`5`), Reason: monitorconfig.ReasonNoDeclaration}},
		}, `{"monitor":"bare","declared":false,"revision":2,"declaration":null,"document":{},"fields":[],"secrets":[],
			"ignored":[{"path":"n","value":5,"secret":false,"reason":"no_declaration"}],"variables":[],"problems":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.fake.view = tc.view

			rec := b.serve(t, http.MethodGet, settingsURL, "", b.session(t, "paulo-santos"))

			if rec.Code != http.StatusOK || !sameJSON(t, rec.Body.String(), tc.want) {
				t.Errorf("GET = %d %s, want 200 %s", rec.Code, shown(t, rec), tc.want)
			}
		})
	}
}

func TestSettingsSaveAnswersWhatChanged(t *testing.T) {
	revision := int64(3)
	cases := []struct {
		name   string
		body   string
		result monitorconfig.SaveResult
		change monitorconfig.Change
		status int
		want   string
	}{
		{
			name: "a save that restarts",
			body: `{"document":{"url":"https://sei.example.invalid","mode":null},"secrets":{"password":"` + secretValue + `","mail.password":null},` +
				`"variables":{"set":{"EXTRA":"on"},"unset":["OLD"],"secret":["EXTRA"]},"delete_ignored":["old"],"revision":3}`,
			result: monitorconfig.SaveResult{Changed: []string{"mode", "url", "password", "env:EXTRA"}, Restarted: true, Request: 3},
			change: monitorconfig.Change{
				Document:      map[string]json.RawMessage{"url": json.RawMessage(`"https://sei.example.invalid"`), "mode": json.RawMessage(`null`)},
				Secrets:       map[string]*string{"password": text(secretValue), "mail.password": nil},
				Variables:     monitorconfig.VariableChange{Set: map[string]string{"EXTRA": "on"}, Unset: []string{"OLD"}, Secret: []string{"EXTRA"}},
				DeleteIgnored: []string{"old"},
				Revision:      &revision,
			},
			status: http.StatusAccepted,
			want:   `{"changed":["mode","url","password","env:EXTRA"],"restarted":true,"request":3}`,
		},
		{
			name:   "a save that changes nothing",
			body:   `{}`,
			status: http.StatusOK,
			want:   `{"changed":[],"restarted":false,"request":0}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.fake.result = tc.result

			rec := b.serve(t, http.MethodPut, settingsURL, tc.body, b.session(t, "paulo-santos"))

			if rec.Code != tc.status || !sameJSON(t, rec.Body.String(), tc.want) {
				t.Errorf("PUT = %d %s, want %d %s", rec.Code, shown(t, rec), tc.status, tc.want)
			}
			if len(b.fake.changes) != 1 || !reflect.DeepEqual(b.fake.changes[0], tc.change) {
				t.Errorf("Save received %d changes, want exactly the body (the first is shown only if it differs)", len(b.fake.changes))
			}
		})
	}
}

func TestSettingsProblemsAre422(t *testing.T) {
	b := newBench(t)
	b.fake.err = &monitorconfig.ProblemsError{Problems: []monitorconfig.Problem{
		{Path: "n", Message: "use a whole number"},
		{Path: "url", Message: "set a value"},
	}}

	rec := b.serve(t, http.MethodPut, settingsURL, `{"document":{"n":"three"}}`, b.session(t, "paulo-santos"))

	want := `{"problems":[{"path":"n","message":"use a whole number"},{"path":"url","message":"set a value"}]}`
	if rec.Code != http.StatusUnprocessableEntity || !sameJSON(t, rec.Body.String(), want) {
		t.Errorf("PUT = %d %s, want 422 %s", rec.Code, shown(t, rec), want)
	}
}

func TestSettingsErrorsAnswerTheirStatus(t *testing.T) {
	everyRoute := []string{"GET settings", "PUT settings", "GET progress"}
	byName := map[string]call{"GET settings": routes[0], "PUT settings": routes[1], "GET progress": routes[2]}
	cases := []struct {
		name   string
		err    error
		routes []string
		status int
		detail string
		// hidden is a part of err the answer leaves out.
		hidden string
	}{
		{"unknown", monitorconfig.ErrUnknown, everyRoute, http.StatusNotFound, "no monitor has this name", ""},
		{"database", errors.Join(monitorconfig.ErrDatabase, errors.New("dial tcp: connection refused")), everyRoute, http.StatusServiceUnavailable,
			"the hub's database cannot be reached", "connection refused"},
		{"busy", monitorconfig.ErrBusy, []string{"PUT settings"}, http.StatusConflict, "another save of this monitor is in progress", ""},
		{"restarting", monitorconfig.ErrRestarting, []string{"PUT settings"}, http.StatusConflict,
			"the monitor is still restarting after the last save; read the progress and try again once it settles", ""},
		{"stale", monitorconfig.ErrStale, []string{"PUT settings"}, http.StatusConflict, "the saved configuration changed since this revision; read it again", ""},
		{"validator", &monitorconfig.ValidatorError{Reason: "it exited with code 2", Stderr: "boom " + secretValue}, []string{"PUT settings"}, http.StatusBadGateway,
			"the monitor could not validate the values: it exited with code 2", "boom"},
		{"anything else", errors.New("something broke"), everyRoute, http.StatusInternalServerError, "internal error", "something broke"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.fake.err = tc.err
			session := b.session(t, "paulo-santos")
			for _, name := range tc.routes {
				r := byName[name]
				rec := b.serve(t, r.method, r.target, r.body, session)
				if rec.Code != tc.status || detail(t, rec) != tc.detail {
					t.Errorf("%s = %d %s, want %d %q", name, rec.Code, shown(t, rec), tc.status, tc.detail)
				}
				if tc.hidden != "" && strings.Contains(rec.Body.String(), tc.hidden) {
					t.Errorf("%s answers with %q from the error", name, tc.hidden)
				}
			}
			if tc.status == http.StatusInternalServerError && !strings.Contains(b.logs.String(), "API request failed") {
				t.Errorf("the logs miss the failed request:\n%s", redacted(b.logs.String()))
			}
		})
	}
}

func TestSettingsProgressShowsTheRestart(t *testing.T) {
	requested := time.Date(2026, 11, 28, 12, 0, 0, 0, time.FixedZone("BRT", -3*60*60))
	cases := []struct {
		name     string
		progress monitorconfig.Progress
		want     string
	}{
		{"none", monitorconfig.Progress{State: monitorconfig.ProgressNone, Phase: supervisor.PhaseNotStarted},
			`{"request":0,"state":"none","requested_at":null,"phase":"not_started","monitor_state":"","message":"","reason":""}`},
		{"restarting", monitorconfig.Progress{State: monitorconfig.ProgressRestarting, Request: 3, RequestedAt: &requested, Phase: supervisor.PhaseStarting},
			`{"request":3,"state":"restarting","requested_at":"2026-11-28T15:00:00Z","phase":"starting","monitor_state":"","message":"","reason":""}`},
		{"up", monitorconfig.Progress{State: monitorconfig.ProgressUp, Request: 3, RequestedAt: &requested, Phase: supervisor.PhaseRunning, MonitorState: monitorpb.State_STATE_IDLE, Message: "ok"},
			`{"request":3,"state":"up","requested_at":"2026-11-28T15:00:00Z","phase":"running","monitor_state":"idle","message":"ok","reason":""}`},
		{"failed", monitorconfig.Progress{State: monitorconfig.ProgressFailed, Request: 3, RequestedAt: &requested, Phase: supervisor.PhaseFailed, Reason: "connecting to [redacted]: refused"},
			`{"request":3,"state":"failed","requested_at":"2026-11-28T15:00:00Z","phase":"failed","monitor_state":"","message":"","reason":"connecting to [redacted]: refused"}`},
		{"no answer", monitorconfig.Progress{State: monitorconfig.ProgressNoAnswer, Request: 3, RequestedAt: &requested, Phase: supervisor.PhaseRunning, MonitorState: monitorpb.State_STATE_CHECKING},
			`{"request":3,"state":"no_answer","requested_at":"2026-11-28T15:00:00Z","phase":"running","monitor_state":"","message":"","reason":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t)
			b.fake.progress = tc.progress

			rec := b.serve(t, http.MethodGet, progressURL, "", b.session(t, "paulo-santos"))

			if rec.Code != http.StatusOK || !sameJSON(t, rec.Body.String(), tc.want) {
				t.Errorf("progress = %d %s, want 200 %s", rec.Code, shown(t, rec), tc.want)
			}
		})
	}
}
