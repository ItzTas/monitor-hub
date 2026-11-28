package pages

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/api/session"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

// fakeAccounts answers like accounts.Service: an unknown cookie is
// ErrUnauthenticated, and any pair but login/password is wrongPassword.
type fakeAccounts struct {
	mu            sync.Mutex
	sessions      map[string]access.Principal
	sessionErr    error
	login         string
	password      string
	session       accounts.Session
	principal     access.Principal
	wrongPassword error
	logoutErr     error
	onLogin       func(login string)
	loggedOut     []string
	calls         map[string]int
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{
		sessions:      map[string]access.Principal{},
		wrongPassword: accounts.ErrWrongPassword,
		calls:         map[string]int{},
	}
}

func (f *fakeAccounts) BySession(_ context.Context, id string) (access.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["BySession"]++
	if f.sessionErr != nil {
		return access.Principal{}, f.sessionErr
	}
	p, ok := f.sessions[id]
	if !ok {
		return access.Principal{}, accounts.ErrUnauthenticated
	}
	return p, nil
}

func (f *fakeAccounts) Login(_ context.Context, login, password string) (accounts.Session, access.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Login"]++
	if f.onLogin != nil {
		f.onLogin(login)
	}
	if login != f.login || password != f.password {
		return accounts.Session{}, access.Principal{}, f.wrongPassword
	}
	return f.session, f.principal, nil
}

func (f *fakeAccounts) Logout(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Logout"]++
	f.loggedOut = append(f.loggedOut, sessionID)
	return f.logoutErr
}

func (f *fakeAccounts) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

type fakeSettings struct {
	mu       sync.Mutex
	view     monitorconfig.View
	saved    monitorconfig.SaveResult
	progress monitorconfig.Progress
	start    monitorconfig.StartReport
	errs     map[string]error
	changes  []monitorconfig.Change
	calls    map[string]int
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{errs: map[string]error{}, calls: map[string]int{}}
}

func (f *fakeSettings) Read(context.Context, string) (monitorconfig.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Read"]++
	return f.view, f.errs["Read"]
}

func (f *fakeSettings) Save(_ context.Context, _ string, change monitorconfig.Change) (monitorconfig.SaveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Save"]++
	f.changes = append(f.changes, change)
	return f.saved, f.errs["Save"]
}

func (f *fakeSettings) Progress(context.Context, string) (monitorconfig.Progress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Progress"]++
	return f.progress, f.errs["Progress"]
}

func (f *fakeSettings) LastStart(context.Context, string) (monitorconfig.StartReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["LastStart"]++
	return f.start, f.errs["LastStart"]
}

func (f *fakeSettings) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// fakeMonitors.List gives only what the principal may view, like monitors.Service.List.
type fakeMonitors struct {
	mu       sync.Mutex
	monitors []monitors.Monitor
	listErr  error
	check    monitors.CheckResult
	checkErr error
	listedBy []access.Principal
	calls    map[string]int
}

func newFakeMonitors() *fakeMonitors {
	return &fakeMonitors{calls: map[string]int{}}
}

func (f *fakeMonitors) List(_ context.Context, p access.Principal) ([]monitors.Monitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["List"]++
	f.listedBy = append(f.listedBy, p)
	if f.listErr != nil {
		return nil, f.listErr
	}
	var shown []monitors.Monitor
	for _, m := range f.monitors {
		if len(access.Visible(p, []string{m.Name})) == 1 {
			shown = append(shown, m)
		}
	}
	return shown, nil
}

func (f *fakeMonitors) Check(context.Context, string) (monitors.CheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Check"]++
	return f.check, f.checkErr
}

func (f *fakeMonitors) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

type fakeAfter struct {
	mu        sync.Mutex
	durations []time.Duration
	called    chan struct{}
	fire      chan time.Time
}

func newFakeAfter() *fakeAfter {
	return &fakeAfter{called: make(chan struct{}, 1), fire: make(chan time.Time)}
}

func (a *fakeAfter) after(d time.Duration) <-chan time.Time {
	a.mu.Lock()
	a.durations = append(a.durations, d)
	a.mu.Unlock()
	select {
	case a.called <- struct{}{}:
	default:
	}
	return a.fire
}

func (a *fakeAfter) all() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.durations)
}

// lockedBuffer collects the logs of requests that may run at the same time.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

const (
	rightPassword = "the right password for paulo-santos"
	wrongPassword = "a wrong password for paulo-santos"
	newSessionID  = "the new session of paulo-santos"
)

// testSessions are the sessions newTestSite knows, by the value of their cookie.
var testSessions = map[string]access.Principal{
	"admin": access.NewSession(access.Person{Login: "paulo-santos", Admin: true}),
	"configurer": access.NewSession(access.Person{Login: "sei-diario-configurer", Granted: []access.Permission{
		{Monitor: "sei", Action: access.Configure},
		{Monitor: "diario", Action: access.Configure},
	}}),
	"viewer": access.NewSession(access.Person{Login: "sei-viewer", Granted: []access.Permission{
		{Monitor: "sei", Action: access.View},
		{Monitor: "sei", Action: access.Check},
	}}),
}

func cookie(name string) *http.Cookie {
	return &http.Cookie{Name: gate.CookieName, Value: name}
}

// newTestSite fills in the fakes it returns. Without deps.Logins a wrong password
// answers at once, and without deps.Log the logs are dropped.
func newTestSite(t *testing.T, deps Deps) (*http.ServeMux, *fakeSettings, *fakeMonitors, *fakeAccounts) {
	t.Helper()
	settings, monitored, people := newFakeSettings(), newFakeMonitors(), newFakeAccounts()
	people.login, people.password = "paulo-santos", rightPassword
	people.session = accounts.Session{ID: newSessionID, ExpiresAt: time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC)}
	people.principal = testSessions["admin"]
	maps.Copy(people.sessions, testSessions)
	deps.Settings, deps.Monitors, deps.Accounts = settings, monitored, people
	if deps.Logins == nil {
		at := newFakeAfter()
		close(at.fire)
		deps.Logins = session.NewLogins(at.after, nil)
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux, settings, monitored, people
}
