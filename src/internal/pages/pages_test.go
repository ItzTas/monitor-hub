package pages

import (
	"context"
	"errors"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/api/session"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

const (
	textNotEncrypted = "This connection is not encrypted, so your password and the session cookie travel in clear text. Use this page only on your local network or through a VPN, or put HTTPS in front of the hub."
	textOrigin       = "The hub refused this request because it came from another site."
	textDatabaseRead = "The hub's database cannot be reached, so the settings cannot be shown."
	textInternal     = "Something went wrong. The hub's log has the details."
	textIncomplete   = "The form is incomplete. Reload the page and try again."
	textTooLarge     = "The form is too large. The hub saved nothing."
	textNoMonitor    = "You cannot change the configuration of any monitor. An admin can give you the configure permission."
)

type option func(*http.Request)

func fromHub(r *http.Request) { r.Header.Set("Origin", "http://example.com") }

func withHeader(name, value string) option {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

func withCookie(c *http.Cookie) option {
	return func(r *http.Request) { r.AddCookie(c) }
}

func serve(ctx context.Context, mux http.Handler, method, target, body string, options ...option) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, o := range options {
		o(r)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func signInForm(login, password string) string {
	return url.Values{"login": {login}, "password": {password}}.Encode()
}

// shown is the body of rec for a failure message. A password in it is an error
// of its own, and the copy shown has it replaced.
func shown(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := rec.Body.String()
	if withoutPasswords(body) != body {
		t.Error("a response body carries a password")
	}
	return withoutPasswords(body)
}

func withoutPasswords(text string) string {
	for _, password := range []string{rightPassword, wrongPassword, url.QueryEscape(wrongPassword)} {
		text = strings.ReplaceAll(text, password, "[password]")
	}
	return text
}

func setCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, line := range rec.Header().Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err == nil && c.Name == gate.CookieName {
			return c
		}
	}
	return nil
}

func redirectsTo(rec *httptest.ResponseRecorder, location string) bool {
	return rec.Code == http.StatusSeeOther && rec.Header().Get("Location") == location
}

// silent is true when the handler wrote nothing for the client.
func silent(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusOK && rec.Body.Len() == 0 && rec.Header().Get("Location") == "" && setCookie(rec) == nil
}

type card struct {
	name string
	html string
}

func cards(rec *httptest.ResponseRecorder) []card {
	var found []card
	for _, m := range regexp.MustCompile(`(?s)<article class="card" id="monitor-([a-z0-9-]+)">(.*?)</article>`).FindAllStringSubmatch(rec.Body.String(), -1) {
		found = append(found, card{name: m[1], html: html.UnescapeString(m[2])})
	}
	return found
}

func cardNames(found []card) []string {
	names := make([]string, 0, len(found))
	for _, c := range found {
		names = append(names, c.name)
	}
	return names
}

func TestTheSignInPage(t *testing.T) {
	mux, _, _, _ := newTestSite(t, Deps{})

	rec := serve(t.Context(), mux, http.MethodGet, "/settings/login", "")

	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `<form method="post" action="/settings/login">`) {
		t.Fatalf("GET /settings/login = %d %s, want 200 with the sign-in form", rec.Code, shown(t, rec))
	}
	login := startTag(body, "login")
	if !hasAttr(login, "name", "login") || !hasAttr(login, "autocomplete", "username") {
		t.Errorf("the login field is %q, want name login and autocomplete username", login)
	}
	password := startTag(body, "password")
	if !hasAttr(password, "name", "password") || !hasAttr(password, "type", "password") || !hasAttr(password, "autocomplete", "current-password") {
		t.Errorf("the password field is %q, want name password, type password and autocomplete current-password", password)
	}
	for _, label := range []string{`<label for="login">`, `<label for="password">`} {
		if !strings.Contains(body, label) {
			t.Errorf("the sign-in page has no %s", label)
		}
	}
	if strings.Contains(body, "<noscript") {
		t.Error("the sign-in page has a <noscript>, but it works without JavaScript")
	}
	if !strings.Contains(text(rec), textNotEncrypted) {
		t.Error("the sign-in page over HTTP does not warn that the connection is not encrypted")
	}

	overHTTPS := serve(t.Context(), mux, http.MethodGet, "/settings/login", "", withHeader("X-Forwarded-Proto", "https"))
	if overHTTPS.Code != http.StatusOK || strings.Contains(text(overHTTPS), textNotEncrypted) {
		t.Errorf("the sign-in page over HTTPS = %d, want 200 without the warning", overHTTPS.Code)
	}

	signedIn := serve(t.Context(), mux, http.MethodGet, "/settings/login", "", withCookie(cookie("admin")))
	if !redirectsTo(signedIn, "/settings") {
		t.Errorf("GET /settings/login with a session = %d to %q, want 303 to /settings", signedIn.Code, signedIn.Header().Get("Location"))
	}

	expired := serve(t.Context(), mux, http.MethodGet, "/settings/login", "", withCookie(cookie("expired")))
	if expired.Code != http.StatusOK || !strings.Contains(expired.Body.String(), `action="/settings/login"`) {
		t.Errorf("GET /settings/login with an expired session = %d, want 200 with the sign-in form", expired.Code)
	}
}

func TestSigningInSetsTheSessionCookie(t *testing.T) {
	after := newFakeAfter()
	mux, _, _, _ := newTestSite(t, Deps{Logins: session.NewLogins(after.after, nil)})

	rec := serve(t.Context(), mux, http.MethodPost, "/settings/login", signInForm("paulo-santos", rightPassword), fromHub)

	if !redirectsTo(rec, "/settings") {
		t.Fatalf("sign in = %d to %q %s, want 303 to /settings", rec.Code, rec.Header().Get("Location"), shown(t, rec))
	}
	c := setCookie(rec)
	if c == nil || c.Value != newSessionID {
		t.Fatal("the sign-in did not set the cookie of the session the accounts opened")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 43200 || c.Secure {
		t.Errorf("the session cookie has HttpOnly %v, SameSite %v, Path %q, Max-Age %d, Secure %v; want HttpOnly, Strict, /, 43200 and no Secure",
			c.HttpOnly, c.SameSite, c.Path, c.MaxAge, c.Secure)
	}

	overHTTPS := serve(t.Context(), mux, http.MethodPost, "/settings/login", signInForm("paulo-santos", rightPassword), fromHub, withHeader("X-Forwarded-Proto", "https"))
	if c := setCookie(overHTTPS); !redirectsTo(overHTTPS, "/settings") || c == nil || !c.Secure {
		t.Errorf("sign in over HTTPS = %d, want 303 with a Secure cookie", overHTTPS.Code)
	}
	if got := after.all(); len(got) != 0 {
		t.Errorf("a right password waited %v, want no wait", got)
	}
}

type journal struct {
	mu      sync.Mutex
	entries []string
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

// within is what ch gives, or a failure saying why after 10 s.
func within[T any](t *testing.T, ch <-chan T, why string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal(why)
		panic("unreachable")
	}
}

// give sends v on ch, or fails saying why after 10 s.
func give[T any](t *testing.T, ch chan<- T, v T, why string) {
	t.Helper()
	select {
	case ch <- v:
	case <-time.After(10 * time.Second):
		t.Fatal(why)
	}
}

func TestAWrongPasswordWaitsInTheQueueAndIsNeverEchoed(t *testing.T) {
	after, logs, notes := newFakeAfter(), &lockedBuffer{}, &journal{}
	waited := make(chan struct{}, 1)
	waiting := func() {
		notes.add("waiting")
		waited <- struct{}{}
	}
	mux, _, _, people := newTestSite(t, Deps{
		Logins: session.NewLogins(after.after, waiting),
		Log:    slog.New(slog.NewTextHandler(logs, nil)),
	})
	people.onLogin = func(login string) { notes.add("login " + login) }
	answeredWrong, answeredRight := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)

	go func() {
		answeredWrong <- serve(t.Context(), mux, http.MethodPost, "/settings/login", signInForm("paulo-santos", wrongPassword), fromHub)
	}()
	within(t, after.called, "the wrong password never reached the 1 s wait: the sign-in skipped the queue")
	select {
	case <-answeredWrong:
		t.Fatal("the wrong password was answered before the delay")
	default:
	}
	go func() {
		answeredRight <- serve(t.Context(), mux, http.MethodPost, "/settings/login", signInForm("paulo-santos", rightPassword), fromHub)
	}()
	within(t, waited, "the second sign-in never waited behind the wrong password: it skipped the queue")
	notes.add("release")
	give(t, after.fire, time.Time{}, "the wrong password never took its 1 s wait from the queue")
	wrong := within(t, answeredWrong, "the wrong password was never answered after the wait")
	right := within(t, answeredRight, "the sign-in that waited was never answered")

	if wrong.Code != http.StatusUnauthorized {
		t.Errorf("the wrong password = %d %s, want 401", wrong.Code, shown(t, wrong))
	}
	body := wrong.Body.String()
	password := startTag(body, "password")
	describedBy, _ := attr(password, "aria-describedby")
	if !hasAttr(password, "aria-invalid", "true") || describedBy == "" {
		t.Errorf("the password field after a wrong password is %q, want aria-invalid and aria-describedby", withoutPasswords(password))
	}
	message := regexp.MustCompile(`(?s)<p\s[^>]*\bid="` + regexp.QuoteMeta(describedBy) + `"[^>]*>(.*?)</p>`).FindStringSubmatch(body)
	if message == nil || !strings.Contains(html.UnescapeString(message[1]), "wrong login or password") {
		t.Errorf("the message the password field points to is %q, want wrong login or password", withoutPasswords(strings.Join(message, " | ")))
	}
	if value, _ := attr(password, "value"); value != "" {
		t.Error("the password field after a wrong password has a value")
	}
	if !hasAttr(startTag(body, "login"), "value", "paulo-santos") {
		t.Errorf("the login field after a wrong password is %q, want the typed login", startTag(body, "login"))
	}
	if strings.Contains(body, wrongPassword) || strings.Contains(body, url.QueryEscape(wrongPassword)) {
		t.Error("the page after a wrong password carries the typed password")
	}
	if setCookie(wrong) != nil {
		t.Error("a wrong password set the session cookie")
	}
	if !redirectsTo(right, "/settings") {
		t.Errorf("the sign-in that waited = %d %s, want 303 to /settings", right.Code, shown(t, right))
	}
	if want := []string{"login paulo-santos", "waiting", "release", "login paulo-santos"}; !slices.Equal(notes.all(), want) {
		t.Errorf("the sign-ins ran as %v, want %v", notes.all(), want)
	}
	if want := []time.Duration{time.Second}; !slices.Equal(after.all(), want) {
		t.Errorf("after was called with %v, want %v (only for the wrong password)", after.all(), want)
	}
	if strings.Contains(logs.String(), wrongPassword) {
		t.Errorf("the log carries the typed password:\n%s", withoutPasswords(logs.String()))
	}

	go func() {
		answeredWrong <- serve(t.Context(), mux, http.MethodPost, "/settings/login", signInForm("paulo-santos", wrongPassword), fromHub)
	}()
	within(t, after.called, "the second wrong password never reached the 1 s wait: the sign-in skipped the queue")
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		answeredRight <- serve(ctx, mux, http.MethodPost, "/settings/login", signInForm("paulo-santos", rightPassword), fromHub)
	}()
	within(t, waited, "the sign-in of the client that leaves never waited in the queue")
	cancel()
	left := within(t, answeredRight, "the client that left the queue was never let go")
	give(t, after.fire, time.Time{}, "the second wrong password never took its 1 s wait from the queue")
	within(t, answeredWrong, "the second wrong password was never answered after the wait")

	if !silent(left) {
		t.Errorf("a client that left the queue got %d %s, want nothing written", left.Code, shown(t, left))
	}
	if got := people.callCount("Login"); got != 3 {
		t.Errorf("Login ran %d times, want 3 (never for the client that left the queue)", got)
	}
}

func TestASignInFormThatCannotBeReadIsRefused(t *testing.T) {
	mux, _, _, people := newTestSite(t, Deps{})

	broken := serve(t.Context(), mux, http.MethodPost, "/settings/login", "login=%zz", fromHub)
	if broken.Code != http.StatusBadRequest || !strings.Contains(text(broken), textIncomplete) {
		t.Errorf("a form with a broken escape = %d %s, want 400 with %q", broken.Code, shown(t, broken), textIncomplete)
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/settings/login", strings.NewReader(signInForm("paulo-santos", rightPassword)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fromHub(r)
	large := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(large, r.Body, 8)
	mux.ServeHTTP(large, r)
	if large.Code != http.StatusRequestEntityTooLarge || !strings.Contains(text(large), textTooLarge) {
		t.Errorf("a form over the body limit = %d %s, want 413 with %q", large.Code, shown(t, large), textTooLarge)
	}

	if got := people.callCount("Login"); got != 0 {
		t.Errorf("Login ran %d times for forms it could not read, want 0", got)
	}
}

func TestSigningInAndOutNeedTheOrigin(t *testing.T) {
	mux, _, _, people := newTestSite(t, Deps{})
	origins := map[string][]option{
		"no Origin":      nil,
		"another origin": {withHeader("Origin", "http://evil.example")},
	}
	routes := map[string]string{
		"/settings/login":  signInForm("paulo-santos", rightPassword),
		"/settings/logout": "",
	}

	for origin, options := range origins {
		for path, body := range routes {
			rec := serve(t.Context(), mux, http.MethodPost, path, body, append(options, withCookie(cookie("admin")))...)
			if rec.Code != http.StatusForbidden || !strings.Contains(text(rec), textOrigin) {
				t.Errorf("POST %s with %s = %d %s, want 403 with %q", path, origin, rec.Code, shown(t, rec), textOrigin)
			}
			if setCookie(rec) != nil {
				t.Errorf("POST %s with %s set the session cookie", path, origin)
			}
		}
	}
	if login, logout := people.callCount("Login"), people.callCount("Logout"); login != 0 || logout != 0 {
		t.Errorf("Login ran %d times and Logout %d times from another site, want 0 and 0", login, logout)
	}
}

func TestSigningOutEndsTheSession(t *testing.T) {
	mux, _, _, people := newTestSite(t, Deps{})

	rec := serve(t.Context(), mux, http.MethodPost, "/settings/logout", "", fromHub, withCookie(cookie("admin")))

	if !redirectsTo(rec, "/settings/login") {
		t.Errorf("sign out = %d to %q, want 303 to /settings/login", rec.Code, rec.Header().Get("Location"))
	}
	if c := setCookie(rec); c == nil || c.MaxAge >= 0 {
		t.Error("sign out did not clear the session cookie")
	}
	if !slices.Equal(people.loggedOut, []string{"admin"}) {
		t.Errorf("Logout got %d session ids, want only the one in the cookie", len(people.loggedOut))
	}

	anonymous := serve(t.Context(), mux, http.MethodPost, "/settings/logout", "", fromHub)
	if c := setCookie(anonymous); !redirectsTo(anonymous, "/settings/login") || c == nil || c.MaxAge >= 0 {
		t.Errorf("sign out without a cookie = %d to %q, want 303 to /settings/login with the cookie cleared", anonymous.Code, anonymous.Header().Get("Location"))
	}
	if got := people.callCount("Logout"); got != 1 {
		t.Errorf("Logout ran %d times, want 1 (not without a cookie)", got)
	}

	people.logoutErr = errors.New("database down")
	failed := serve(t.Context(), mux, http.MethodPost, "/settings/logout", "", fromHub, withCookie(cookie("admin")))
	if failed.Code != http.StatusInternalServerError || !strings.Contains(text(failed), textInternal) || setCookie(failed) != nil {
		t.Errorf("sign out with the database down = %d %s, want 500 with %q and no cookie", failed.Code, shown(t, failed), textInternal)
	}
}

func TestPagesNeedASession(t *testing.T) {
	mux, _, monitored, _ := newTestSite(t, Deps{})
	toSignIn := map[string][]option{
		"no cookie":           nil,
		"an unknown cookie":   {withCookie(cookie("unknown"))},
		"only a bearer token": {withHeader("Authorization", "Bearer mht_x")},
	}

	for name, options := range toSignIn {
		rec := serve(t.Context(), mux, http.MethodGet, "/settings", "", options...)
		if !redirectsTo(rec, "/settings/login") {
			t.Errorf("GET /settings with %s = %d to %q, want 303 to /settings/login", name, rec.Code, rec.Header().Get("Location"))
		}
		htmx := serve(t.Context(), mux, http.MethodGet, "/settings", "", append(options, withHeader("HX-Request", "true"))...)
		if htmx.Code != http.StatusUnauthorized || htmx.Body.Len() != 0 || htmx.Header().Get("HX-Redirect") != "/settings/login" {
			t.Errorf("htmx GET /settings with %s = %d %q, HX-Redirect %q; want 401, no body and /settings/login",
				name, htmx.Code, shown(t, htmx), htmx.Header().Get("HX-Redirect"))
		}
	}
	if got := monitored.callCount("List"); got != 0 {
		t.Errorf("List ran %d times without a session, want 0", got)
	}

	broken, _, _, people := newTestSite(t, Deps{})
	people.sessionErr = errors.New("database down")
	rec := serve(t.Context(), broken, http.MethodGet, "/settings", "", withCookie(cookie("admin")))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(text(rec), textDatabaseRead) {
		t.Errorf("GET /settings with the database down = %d %s, want 503 with %q", rec.Code, shown(t, rec), textDatabaseRead)
	}
}

func TestTheMonitorListShowsWhatThePersonMayConfigure(t *testing.T) {
	entries := []catalog.Entry{
		{Manifest: catalog.Manifest{Name: "anhanguera"}, Settings: &settings.Declaration{}},
		{Manifest: catalog.Manifest{Name: "diario"}, Settings: &settings.Declaration{}},
		{Manifest: catalog.Manifest{Name: "sei"}},
	}
	mux, _, monitored, _ := newTestSite(t, Deps{Entries: entries})
	monitored.monitors = []monitors.Monitor{{Name: "diario", DisplayName: "Diario", Running: true, State: "idle", Message: "ok"}}

	admin := serve(t.Context(), mux, http.MethodGet, "/settings", "", withCookie(cookie("admin")))
	found := cards(admin)
	if admin.Code != http.StatusOK || !slices.Equal(cardNames(found), []string{"anhanguera", "diario", "sei"}) {
		t.Fatalf("the list of an admin = %d with cards %v, want 200 with anhanguera, diario and sei", admin.Code, cardNames(found))
	}
	diario := found[1].html
	for _, want := range []string{"Diario", "Running", "Idle", "ok", `href="/settings/diario"`} {
		if !strings.Contains(diario, want) {
			t.Errorf("the card of diario has no %q:\n%s", want, diario)
		}
	}
	if strings.Contains(diario, "Declares no settings") {
		t.Errorf("the card of diario says it declares no settings:\n%s", diario)
	}
	if !strings.Contains(found[2].html, "Declares no settings") {
		t.Errorf("the card of sei does not say it declares no settings:\n%s", found[2].html)
	}

	configurer := serve(t.Context(), mux, http.MethodGet, "/settings", "", withCookie(cookie("configurer")))
	found = cards(configurer)
	if configurer.Code != http.StatusOK || !slices.Equal(cardNames(found), []string{"diario", "sei"}) {
		t.Fatalf("the list of sei-diario-configurer = %d with cards %v, want 200 with diario and sei", configurer.Code, cardNames(found))
	}
	for _, c := range found {
		if !strings.Contains(c.html, `href="/settings/`+c.name+`"`) {
			t.Errorf("the card of %s does not link to its page:\n%s", c.name, c.html)
		}
		if strings.Contains(c.html, "Running") || strings.Contains(c.html, "Not running") || strings.Contains(c.html, "Diario") {
			t.Errorf("the card of %s shows the state of a monitor sei-diario-configurer may not view:\n%s", c.name, c.html)
		}
	}
	if !strings.Contains(found[1].html, "Declares no settings") {
		t.Errorf("the card of sei does not say it declares no settings:\n%s", found[1].html)
	}
	monitored.mu.Lock()
	listedBy := monitored.listedBy[len(monitored.listedBy)-1]
	monitored.mu.Unlock()
	if listedBy.Login() != "sei-diario-configurer" {
		t.Errorf("List got the principal of %q, want sei-diario-configurer", listedBy.Login())
	}

	viewer := serve(t.Context(), mux, http.MethodGet, "/settings", "", withCookie(cookie("viewer")))
	if viewer.Code != http.StatusOK || len(cards(viewer)) != 0 || !strings.Contains(text(viewer), textNoMonitor) {
		t.Errorf("the list of sei-viewer = %d with cards %v, want 200 with no card and %q", viewer.Code, cardNames(cards(viewer)), textNoMonitor)
	}

	monitored.listErr = errors.New("database down")
	failed := serve(t.Context(), mux, http.MethodGet, "/settings", "", withCookie(cookie("admin")))
	if failed.Code != http.StatusServiceUnavailable || !strings.Contains(text(failed), textDatabaseRead) {
		t.Errorf("the list with List failing = %d %s, want 503 with %q", failed.Code, shown(t, failed), textDatabaseRead)
	}
}

func TestAClientThatLeftGetsNothing(t *testing.T) {
	logs := &lockedBuffer{}
	mux, configs, monitored, people := newTestSite(t, Deps{
		Entries: []catalog.Entry{{Manifest: catalog.Manifest{Name: "sei"}}},
		Log:     slog.New(slog.NewTextHandler(logs, nil)),
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// In this order: once the session cannot be read, nothing after it is reached.
	failures := []struct {
		name    string
		fail    func()
		request func() *httptest.ResponseRecorder
	}{
		{"List fails", func() { monitored.listErr = errors.New("database down") }, func() *httptest.ResponseRecorder {
			return serve(ctx, mux, http.MethodGet, "/settings", "", withCookie(cookie("admin")))
		}},
		{"Logout fails", func() { people.logoutErr = errors.New("database down") }, func() *httptest.ResponseRecorder {
			return serve(ctx, mux, http.MethodPost, "/settings/logout", "", fromHub, withCookie(cookie("admin")))
		}},
		{"the monitor cannot be read", func() { configs.errs["Read"] = errors.New("database down") }, func() *httptest.ResponseRecorder {
			return serve(ctx, mux, http.MethodGet, "/settings/sei", "", withCookie(cookie("admin")))
		}},
		{"the monitor cannot be read for a list edit", func() { configs.errs["Read"] = errors.New("database down") }, func() *httptest.ResponseRecorder {
			return serve(ctx, mux, http.MethodPost, "/settings/sei/form", "declaration=x&op=add:labels", fromHub, withCookie(cookie("admin")))
		}},
		{"the progress cannot be read", func() { configs.errs["Progress"] = errors.New("database down") }, func() *httptest.ResponseRecorder {
			return serve(ctx, mux, http.MethodGet, "/settings/sei/progress", "", withCookie(cookie("admin")), withHeader("HX-Request", "true"))
		}},
		{"the check fails", func() { monitored.checkErr = errors.New("deadline exceeded") }, func() *httptest.ResponseRecorder {
			return serve(ctx, mux, http.MethodPost, "/settings/sei/check", "", fromHub, withCookie(cookie("admin")), withHeader("HX-Request", "true"))
		}},
		{"the session cannot be read", func() { people.sessionErr = errors.New("database down") }, func() *httptest.ResponseRecorder {
			return serve(ctx, mux, http.MethodGet, "/settings", "", withCookie(cookie("admin")))
		}},
	}

	for _, f := range failures {
		f.fail()
		if rec := f.request(); !silent(rec) {
			t.Errorf("when %s for a client that left, the page wrote %d %s, want nothing", f.name, rec.Code, shown(t, rec))
		}
	}
	if logs.String() != "" {
		t.Errorf("the failures of clients that left were logged:\n%s", logs.String())
	}
}

func TestEveryPageCarriesTheSecurityHeaders(t *testing.T) {
	mux, _, _, _ := newTestSite(t, Deps{})
	responses := map[string]*httptest.ResponseRecorder{
		"the sign-in page":    serve(t.Context(), mux, http.MethodGet, "/settings/login", ""),
		"the list":            serve(t.Context(), mux, http.MethodGet, "/settings", "", withCookie(cookie("admin"))),
		"the stylesheet":      serve(t.Context(), mux, http.MethodGet, "/static/style.css", ""),
		"a wrong password":    serve(t.Context(), mux, http.MethodPost, "/settings/login", signInForm("paulo-santos", wrongPassword), fromHub),
		"another site's form": serve(t.Context(), mux, http.MethodPost, "/settings/logout", ""),
	}
	want := map[string]string{
		"Content-Security-Policy":    contentSecurityPolicy,
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "same-origin",
		"Cross-Origin-Opener-Policy": "same-origin",
	}

	for name, rec := range responses {
		for header, value := range want {
			if got := rec.Header().Get(header); got != value {
				t.Errorf("%s (%d) has %s %q, want %q", name, rec.Code, header, got, value)
			}
		}
	}
}

func TestTheLayoutLoadsOnlyItsOwnFiles(t *testing.T) {
	mux, _, _, _ := newTestSite(t, Deps{})

	rec := serve(t.Context(), mux, http.MethodGet, "/settings", "", withCookie(cookie("admin")))

	body := rec.Body.String()
	scripts := regexp.MustCompile(`<script\b[^>]*>`).FindAllString(body, -1)
	if len(scripts) != 1 || !strings.Contains(scripts[0], `src="/static/htmx.min.js?v=`) || !strings.Contains(scripts[0], " defer") {
		t.Errorf("the page loads the scripts %q, want only htmx from /static, deferred", scripts)
	}
	if strings.Contains(body, "<style") || strings.Contains(body, " style=") {
		t.Error("the page has inline styles")
	}
	if handlers := regexp.MustCompile(`<[^>]*\son[a-z]+=`).FindAllString(body, -1); len(handlers) != 0 {
		t.Errorf("the page has inline event handlers: %q", handlers)
	}
	if !strings.Contains(body, `<span class="site-login">paulo-santos</span>`) || !strings.Contains(body, `<form method="post" action="/settings/logout">`) {
		t.Errorf("the header has no login of paulo-santos and sign-out form:\n%s", shown(t, rec))
	}

	signIn := serve(t.Context(), mux, http.MethodGet, "/settings/login", "")
	if strings.Contains(signIn.Body.String(), `action="/settings/logout"`) {
		t.Error("the sign-in page without a session has the sign-out form")
	}
}
