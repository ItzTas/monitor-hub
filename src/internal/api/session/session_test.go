package session_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/accounts/accountstest"
	"github.com/ItzTas/monitor-hub/src/internal/api"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/session"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

const (
	wrongPassword = "a wrong password for paulo-santos"
	pauloMe       = `{"login":"paulo-santos","admin":false,"permissions":["monitor:sei:view"],"monitors":{"sei":["view"]}}`
)

var testNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

type journal struct {
	mu      sync.Mutex
	entries []string
	onLogin func(login string) // set before the first request
}

func (j *journal) add(entry string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, entry)
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.entries)
}

// recorder notes each call to Login before the service runs it.
type recorder struct {
	*accounts.Service
	journal *journal
}

func (r recorder) Login(ctx context.Context, login, password string) (accounts.Session, access.Principal, error) {
	r.journal.add("login " + login)
	if r.journal.onLogin != nil {
		r.journal.onLogin(login)
	}
	return r.Service.Login(ctx, login, password)
}

type fakeAfter struct {
	mu        sync.Mutex
	durations []time.Duration
	called    chan struct{}
	fire      chan time.Time
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

type bench struct {
	mux     *http.ServeMux
	logs    *lockedBuffer
	svc     *accounts.Service
	journal *journal
	after   *fakeAfter
	waited  chan struct{}
}

func newBench(t *testing.T) *bench {
	t.Helper()
	b := &bench{
		mux:     http.NewServeMux(),
		logs:    &lockedBuffer{},
		svc:     accounts.NewService(coretest.Pool(t), func() time.Time { return testNow }),
		journal: &journal{},
		after:   &fakeAfter{called: make(chan struct{}, 1), fire: make(chan time.Time)},
		waited:  make(chan struct{}, 1),
	}
	accountstest.Person(t, b.svc, "paulo-santos", "monitor:sei:view")
	accountstest.Person(t, b.svc, "ana-lima")
	humaAPI := humago.New(b.mux, api.HumaConfig(slog.New(slog.NewTextHandler(b.logs, nil)), "test", "test"))
	humaAPI.UseMiddleware(gate.Middleware(humaAPI, b.svc, ""))
	waiting := func() {
		b.journal.add("waiting")
		b.waited <- struct{}{}
	}
	session.RegisterRoutes(humaAPI, session.NewHandler(recorder{Service: b.svc, journal: b.journal}, session.NewLogins(b.after.after, waiting)))
	return b
}

type edit func(*http.Request)

func withCookie(id string) edit {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: gate.CookieName, Value: id}) }
}

func signIn(t *testing.T, login, password string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"login": login, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func (b *bench) serve(ctx context.Context, method, path, body string, edits ...edit) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
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

// shown is the body of rec for a failure message. A password in it is an error
// of its own, and the copy shown has it replaced.
func shown(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := rec.Body.String()
	if redacted(body) != body {
		t.Error("a response body carries a password")
	}
	return redacted(body)
}

func redacted(text string) string {
	for _, password := range []string{accountstest.Password, wrongPassword} {
		text = strings.ReplaceAll(text, password, "[password]")
	}
	return text
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, line := range rec.Header().Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err != nil {
			t.Fatalf("parse Set-Cookie: %v", err)
		}
		if c.Name == gate.CookieName {
			return c
		}
	}
	t.Fatal("no session cookie in the response")
	return nil
}

func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("decode %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(g)
	wantJSON, _ := json.Marshal(w)
	return string(gotJSON) == string(wantJSON)
}

func TestSignInSetsTheCookie(t *testing.T) {
	b := newBench(t)

	rec := b.serve(t.Context(), http.MethodPost, "/api/session", signIn(t, "paulo-santos", accountstest.Password))

	if rec.Code != http.StatusOK || !sameJSON(t, rec.Body.String(), pauloMe) {
		t.Fatalf("sign in = %d %s, want 200 %s", rec.Code, shown(t, rec), pauloMe)
	}
	c := sessionCookie(t, rec)
	if c.Value == "" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 43200 || c.Path != "/" || c.Secure {
		t.Errorf("the session cookie has HttpOnly %v, SameSite %v, Max-Age %d, Path %q, Secure %v; want HttpOnly, Strict, 43200, / and no Secure",
			c.HttpOnly, c.SameSite, c.MaxAge, c.Path, c.Secure)
	}
	me := b.serve(t.Context(), http.MethodGet, "/api/me", "", withCookie(c.Value))
	if me.Code != http.StatusOK || !sameJSON(t, me.Body.String(), pauloMe) {
		t.Errorf("GET /api/me with the new cookie = %d %s, want 200 %s", me.Code, shown(t, me), pauloMe)
	}

	secure := map[string]edit{
		"X-Forwarded-Proto: https": func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") },
		"TLS":                      func(r *http.Request) { r.TLS = &tls.ConnectionState{} },
	}
	for name, e := range secure {
		rec := b.serve(t.Context(), http.MethodPost, "/api/session", signIn(t, "paulo-santos", accountstest.Password), e)
		if rec.Code != http.StatusOK || !sessionCookie(t, rec).Secure {
			t.Errorf("sign in over %s = %d, want 200 with a Secure cookie", name, rec.Code)
		}
	}
}

func TestSignOutEndsTheSession(t *testing.T) {
	b := newBench(t)
	id := accountstest.Session(t, b.svc, "paulo-santos")

	rec := b.serve(t.Context(), http.MethodDelete, "/api/session", "", withCookie(id))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign out = %d %s, want 204", rec.Code, shown(t, rec))
	}
	if c := sessionCookie(t, rec); c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("sign out sets a session cookie with Max-Age %d, want it cleared", c.MaxAge)
	}
	if me := b.serve(t.Context(), http.MethodGet, "/api/me", "", withCookie(id)); me.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/me after signing out = %d, want 401", me.Code)
	}
}

func TestMeNeedsASession(t *testing.T) {
	b := newBench(t)
	token := accountstest.Token(t, b.svc, "paulo-santos", "monitor:sei:view")
	callers := map[string]struct {
		edits []edit
		want  int
	}{
		"a token": {[]edit{func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }}, http.StatusForbidden},
		"nobody":  {nil, http.StatusUnauthorized},
	}
	for name, c := range callers {
		if rec := b.serve(t.Context(), http.MethodGet, "/api/me", "", c.edits...); rec.Code != c.want {
			t.Errorf("GET /api/me by %s = %d, want %d", name, rec.Code, c.want)
		}
	}
}

func TestWrongPasswordsWaitAndQueue(t *testing.T) {
	b := newBench(t)
	answeredA, answeredB := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)

	wrong, right := signIn(t, "paulo-santos", wrongPassword), signIn(t, "ana-lima", accountstest.Password)

	go func() { answeredA <- b.serve(t.Context(), http.MethodPost, "/api/session", wrong) }()
	<-b.after.called
	select {
	case <-answeredA:
		t.Fatal("the wrong password was answered before the delay")
	default:
	}
	go func() { answeredB <- b.serve(t.Context(), http.MethodPost, "/api/session", right) }()
	<-b.waited
	b.journal.add("release")
	b.after.fire <- testNow
	recA, recB := <-answeredA, <-answeredB

	if recA.Code != http.StatusUnauthorized || !strings.Contains(recA.Body.String(), `"wrong login or password"`) {
		t.Errorf("the wrong password = %d %s, want 401 wrong login or password", recA.Code, shown(t, recA))
	}
	if recB.Code != http.StatusOK {
		t.Errorf("the right password = %d %s, want 200", recB.Code, shown(t, recB))
	}
	if want := []string{"login paulo-santos", "waiting", "release", "login ana-lima"}; !slices.Equal(b.journal.all(), want) {
		t.Errorf("the sign-ins ran as %v, want %v", b.journal.all(), want)
	}
	if want := []time.Duration{time.Second}; !slices.Equal(b.after.all(), want) {
		t.Errorf("after was called with %v, want %v (only for the wrong password)", b.after.all(), want)
	}
}

func TestAClientThatLeavesIsNotAServerError(t *testing.T) {
	b := newBench(t)
	ctxA, cancelA := context.WithCancel(t.Context())
	ctxB, cancelB := context.WithCancel(t.Context())
	ctxC, cancelC := context.WithCancel(t.Context())
	b.journal.onLogin = func(login string) {
		if login == "ana-lima" {
			cancelC()
		}
	}
	answeredA, answeredB := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	wrong, right := signIn(t, "paulo-santos", wrongPassword), signIn(t, "paulo-santos", accountstest.Password)

	go func() { answeredA <- b.serve(ctxA, http.MethodPost, "/api/session", wrong) }()
	<-b.after.called
	go func() { answeredB <- b.serve(ctxB, http.MethodPost, "/api/session", right) }()
	<-b.waited
	cancelB()
	recB := <-answeredB
	cancelA()
	recA := <-answeredA
	recC := b.serve(ctxC, http.MethodPost, "/api/session", signIn(t, "ana-lima", accountstest.Password))

	gone := map[string]*httptest.ResponseRecorder{"while queued": recB, "during the sign-in": recC}
	for name, rec := range gone {
		if rec.Code != 499 {
			t.Errorf("a client gone %s got %d %s, want 499", name, rec.Code, shown(t, rec))
		}
	}
	if recA.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password whose client left during the delay = %d %s, want 401", recA.Code, shown(t, recA))
	}
	if want := []string{"login paulo-santos", "waiting", "login ana-lima"}; !slices.Equal(b.journal.all(), want) {
		t.Errorf("the sign-ins ran as %v, want %v", b.journal.all(), want)
	}
	if strings.Contains(b.logs.String(), "API request failed") {
		t.Errorf("a client that left was logged as a failure:\n%s", redacted(b.logs.String()))
	}
	if rec := b.serve(t.Context(), http.MethodPost, "/api/session", right); rec.Code != http.StatusOK {
		t.Errorf("a sign-in after the others left = %d %s, want 200", rec.Code, shown(t, rec))
	}
}

func TestTheResponsesNeverCarryThePassword(t *testing.T) {
	b := newBench(t)
	close(b.after.fire)
	bodies := map[string]struct {
		body     string
		password string
	}{
		"the right password":           {signIn(t, "paulo-santos", accountstest.Password), accountstest.Password},
		"a wrong password":             {signIn(t, "paulo-santos", wrongPassword), wrongPassword},
		"an unknown login":             {signIn(t, "nobody-here", wrongPassword), wrongPassword},
		"no login":                     {`{"password":"` + wrongPassword + `"}`, wrongPassword},
		"a login of the wrong type":    {`{"login":7,"password":"` + wrongPassword + `"}`, wrongPassword},
		"an unknown field":             {`{"login":"paulo-santos","password":"` + wrongPassword + `","extra":true}`, wrongPassword},
		"a password of the wrong type": {`{"login":"paulo-santos","password":["` + wrongPassword + `"]}`, wrongPassword},
		"an array body":                {`["paulo-santos","` + wrongPassword + `"]`, wrongPassword},
		"a string body":                {`"` + wrongPassword + `"`, wrongPassword},
	}
	for name, c := range bodies {
		rec := b.serve(t.Context(), http.MethodPost, "/api/session", c.body)
		if name != "the right password" && (rec.Code < 400 || rec.Code >= 500) {
			t.Errorf("sign in with %s = %d, want a 4xx", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), c.password) {
			t.Errorf("the response body to %s carries the password", name)
		}
		for header, values := range rec.Header() {
			if strings.Contains(strings.Join(values, " "), c.password) {
				t.Errorf("the %s header of the response to %s carries the password", header, name)
			}
		}
	}
}
