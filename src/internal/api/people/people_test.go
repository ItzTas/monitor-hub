package people_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/accounts/accountstest"
	"github.com/ItzTas/monitor-hub/src/internal/api"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/people"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

const (
	anaPassword  = "ana lima first password"
	newPassword  = "ana lima second password"
	weakPassword = "too short"
)

var (
	testNow      = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	longPassword = strings.Repeat("a long password ", 17)
	passwords    = []string{accountstest.Password, anaPassword, newPassword, weakPassword, longPassword}
	itemFields   = []string{"admin", "created_at", "disabled", "login", "permissions"}
)

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
	mux  *http.ServeMux
	pool *pgxpool.Pool
	svc  *accounts.Service
	logs *lockedBuffer
}

// newBench has paulo-santos as the only admin and sei as a monitor.
func newBench(t *testing.T) *bench {
	t.Helper()
	pool := coretest.Pool(t)
	b := &bench{
		mux:  http.NewServeMux(),
		pool: pool,
		svc:  accounts.NewService(pool, func() time.Time { return testNow }),
		logs: &lockedBuffer{},
	}
	if err := b.svc.Prepare(t.Context(), []string{"sei"}); err != nil {
		t.Fatal(err)
	}
	accountstest.Admin(t, b.svc, "paulo-santos")
	now := func(time.Duration) <-chan time.Time {
		c := make(chan time.Time, 1)
		c <- testNow
		return c
	}
	api.RegisterAccess(b.mux, api.AccessDeps{Accounts: b.svc, Log: slog.New(slog.NewTextHandler(b.logs, nil)), After: now})
	return b
}

type edit func(*http.Request)

func withSession(id string) edit {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: gate.CookieName, Value: id}) }
}

func withBearer(secret string) edit {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func serve(ctx context.Context, mux *http.ServeMux, method, path, body string, edits ...edit) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	request.Header.Set("Origin", "http://example.com")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, e := range edits {
		e(request)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, request)
	return rec
}

func (b *bench) serve(t *testing.T, method, path, body string, edits ...edit) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t.Context(), b.mux, method, path, body, edits...)
}

// shown is the body of rec for a failure message, with every test password hidden.
func shown(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := rec.Body.String()
	if redacted(body) != body {
		t.Error("a response body carries a password")
	}
	return redacted(body)
}

func redacted(text string) string {
	for _, password := range passwords {
		text = strings.ReplaceAll(text, password, "[password]")
	}
	return text
}

func jsonBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func detail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var model struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &model); err != nil {
		t.Fatalf("decode the error %s: %v", shown(t, rec), err)
	}
	return model.Detail
}

type item struct {
	Login       string    `json:"login"`
	Admin       bool      `json:"admin"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
	Permissions []string  `json:"permissions"`
}

// decodeItem reads a person from rec, failing unless it has exactly the item fields.
func decodeItem(t *testing.T, rec *httptest.ResponseRecorder) item {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode a person %s: %v", shown(t, rec), err)
	}
	if got := slices.Sorted(maps.Keys(raw)); !slices.Equal(got, itemFields) {
		t.Errorf("a person has the fields %v, want %v", got, itemFields)
	}
	var got item
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	got.CreatedAt = got.CreatedAt.UTC()
	return got
}

// person is login as ListUsers has it.
func (b *bench) person(t *testing.T, login string) (accounts.User, bool) {
	t.Helper()
	users, err := b.svc.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Login == login {
			return u, true
		}
	}
	return accounts.User{}, false
}

func (b *bench) admins(t *testing.T) []string {
	t.Helper()
	users, err := b.svc.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, u := range users {
		if u.Admin && !u.Disabled {
			out = append(out, u.Login)
		}
	}
	return out
}

func (b *bench) signIn(t *testing.T, login, password string) *httptest.ResponseRecorder {
	t.Helper()
	return b.serve(t, http.MethodPost, "/api/session", jsonBody(t, map[string]any{"login": login, "password": password}))
}

func TestPeopleNeedAnAdmin(t *testing.T) {
	b := newBench(t)
	accountstest.Person(t, b.svc, "ana-lima")
	before, err := b.svc.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ana := accountstest.Session(t, b.svc, "ana-lima")
	token := accountstest.Token(t, b.svc, "paulo-santos", "monitor:sei:view")

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/users", ""},
		{http.MethodPost, "/api/users", jsonBody(t, map[string]any{"login": "joana-reis", "password": newPassword, "admin": true})},
		{http.MethodPatch, "/api/users/paulo-santos", `{"admin":false}`},
		{http.MethodPut, "/api/users/paulo-santos/password", jsonBody(t, map[string]any{"password": newPassword})},
		{http.MethodPut, "/api/users/ana-lima/permissions", `{"permissions":["monitor:*:configure"]}`},
		{http.MethodDelete, "/api/users/paulo-santos", ""},
	}
	callers := []struct {
		name   string
		edits  []edit
		status int
		detail string
	}{
		{"a session without admin", []edit{withSession(ana)}, http.StatusForbidden, "you do not have permission for this"},
		{"an admin's token", []edit{withBearer(token)}, http.StatusForbidden, "this route needs a signed-in session"},
		{"nobody", nil, http.StatusUnauthorized, "sign in or send a personal token"},
	}
	for _, c := range callers {
		for _, r := range routes {
			rec := b.serve(t, r.method, r.path, r.body, c.edits...)
			if rec.Code != c.status || detail(t, rec) != c.detail {
				t.Errorf("%s %s by %s = %d %s, want %d %q", r.method, r.path, c.name, rec.Code, shown(t, rec), c.status, c.detail)
			}
		}
	}

	after, err := b.svc.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("the people after refused requests = %+v, want %+v", after, before)
	}
	if _, err := b.svc.ByToken(t.Context(), token); err != nil {
		t.Errorf("the token used on refused requests stopped working: %v", err)
	}
}

func TestCreateDisableAndDelete(t *testing.T) {
	b := newBench(t)
	paulo := accountstest.Session(t, b.svc, "paulo-santos")

	created := b.serve(t, http.MethodPost, "/api/users", jsonBody(t, map[string]any{"login": "ana-lima", "password": anaPassword}), withSession(paulo))
	if created.Code != http.StatusCreated {
		t.Fatalf("create ana-lima = %d %s, want 201", created.Code, shown(t, created))
	}
	ana := item{Login: "ana-lima", CreatedAt: testNow, Permissions: []string{}}
	if got := decodeItem(t, created); !reflect.DeepEqual(got, ana) {
		t.Errorf("the created person = %+v, want %+v", got, ana)
	}
	listed := b.serve(t, http.MethodGet, "/api/users", "", withSession(paulo))
	var page struct {
		Items []item `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); listed.Code != http.StatusOK || err != nil {
		t.Fatalf("GET /api/users = %d %s (%v), want 200", listed.Code, shown(t, listed), err)
	}
	// TestMain runs the package in a zone other than UTC.
	var times struct {
		Items []struct {
			CreatedAt string `json:"created_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &times); err != nil {
		t.Fatal(err)
	}
	for _, u := range times.Items {
		if !strings.HasSuffix(u.CreatedAt, "Z") {
			t.Errorf("GET /api/users gives created_at %q, want a time in UTC", u.CreatedAt)
		}
	}
	paulosItem := item{Login: "paulo-santos", Admin: true, CreatedAt: testNow, Permissions: []string{}}
	if want := []item{ana, paulosItem}; !reflect.DeepEqual(page.Items, want) {
		t.Errorf("GET /api/users = %+v, want %+v", page.Items, want)
	}

	if rec := b.serve(t, http.MethodPut, "/api/users/ana-lima/permissions", `{"permissions":["monitor:sei:view"]}`, withSession(paulo)); rec.Code != http.StatusOK {
		t.Fatalf("give ana-lima monitor:sei:view = %d %s, want 200", rec.Code, shown(t, rec))
	}
	ana.Permissions = []string{"monitor:sei:view"}
	session, principal, err := b.svc.Login(t.Context(), "ana-lima", anaPassword)
	if err != nil {
		t.Fatalf("ana-lima signing in after she was created: %v", err)
	}
	_, token, err := b.svc.CreateToken(t.Context(), principal, accounts.TokenRequest{Label: "rss", Permissions: []access.Permission{{Monitor: "sei", Action: access.View}}})
	if err != nil {
		t.Fatalf("create a token of ana-lima: %v", err)
	}

	disable := b.serve(t, http.MethodPatch, "/api/users/ana-lima", `{"disabled":true}`, withSession(paulo))
	if disable.Code != http.StatusOK {
		t.Fatalf("disable ana-lima = %d %s, want 200", disable.Code, shown(t, disable))
	}
	ana.Disabled = true
	if got := decodeItem(t, disable); !reflect.DeepEqual(got, ana) {
		t.Errorf("the disabled person = %+v, want %+v", got, ana)
	}
	if rec := b.signIn(t, "ana-lima", anaPassword); rec.Code != http.StatusUnauthorized {
		t.Errorf("ana-lima signing in while disabled = %d %s, want 401", rec.Code, shown(t, rec))
	}
	if rec := b.serve(t, http.MethodGet, "/api/me", "", withSession(session.ID)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the session ana-lima opened before she was disabled = %d %s, want 401", rec.Code, shown(t, rec))
	}
	if rec := b.serve(t, http.MethodGet, "/api/me", "", withBearer(token)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the token of ana-lima while she is disabled = %d %s, want 401", rec.Code, shown(t, rec))
	}

	enable := b.serve(t, http.MethodPatch, "/api/users/ana-lima", `{"disabled":false}`, withSession(paulo))
	ana.Disabled = false
	if enable.Code != http.StatusOK || !reflect.DeepEqual(decodeItem(t, enable), ana) {
		t.Errorf("enable ana-lima = %d %s, want 200 with %+v", enable.Code, shown(t, enable), ana)
	}
	if rec := b.serve(t, http.MethodGet, "/api/me", "", withSession(session.ID)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the session ana-lima opened before she was disabled, once she is enabled again = %d %s, want 401", rec.Code, shown(t, rec))
	}
	// A route that needs a session answers a token that works with 403.
	if rec := b.serve(t, http.MethodGet, "/api/me", "", withBearer(token)); rec.Code != http.StatusForbidden || detail(t, rec) != "this route needs a signed-in session" {
		t.Errorf("the token of ana-lima once she is enabled again = %d %s, want 403", rec.Code, shown(t, rec))
	}
	if rec := b.signIn(t, "ana-lima", anaPassword); rec.Code != http.StatusOK {
		t.Errorf("ana-lima signing in once enabled again = %d %s, want 200", rec.Code, shown(t, rec))
	}
	later, _, err := b.svc.Login(t.Context(), "ana-lima", anaPassword)
	if err != nil {
		t.Fatalf("ana-lima signing in before she is deleted: %v", err)
	}

	if rec := b.serve(t, http.MethodDelete, "/api/users/ana-lima", "", withSession(paulo)); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("delete ana-lima = %d %s, want 204 with no body", rec.Code, shown(t, rec))
	}
	gone := []struct{ method, path, body string }{
		{http.MethodDelete, "/api/users/ana-lima", ""},
		{http.MethodPatch, "/api/users/ana-lima", `{"disabled":false}`},
		{http.MethodPut, "/api/users/ana-lima/password", jsonBody(t, map[string]any{"password": newPassword})},
		{http.MethodPut, "/api/users/ana-lima/permissions", `{"permissions":[]}`},
	}
	for _, r := range gone {
		rec := b.serve(t, r.method, r.path, r.body, withSession(paulo))
		if rec.Code != http.StatusNotFound || detail(t, rec) != accounts.ErrUnknownUser.Error() {
			t.Errorf("%s %s once ana-lima is deleted = %d %s, want 404 %q", r.method, r.path, rec.Code, shown(t, rec), accounts.ErrUnknownUser)
		}
	}
	if _, ok := b.person(t, "ana-lima"); ok {
		t.Error("ana-lima is still listed after she was deleted")
	}
	if rec := b.serve(t, http.MethodGet, "/api/me", "", withSession(later.ID)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the session ana-lima opened before she was deleted = %d %s, want 401", rec.Code, shown(t, rec))
	}
}

func TestPermissionsAreReplacedAndApplied(t *testing.T) {
	b := newBench(t)
	if err := b.svc.Prepare(t.Context(), []string{"diario", "sei-b"}); err != nil {
		t.Fatal(err)
	}
	accountstest.Person(t, b.svc, "ana-lima", "monitor:diario:check")
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	ana := accountstest.Session(t, b.svc, "ana-lima")
	monitors := func() map[string][]string {
		t.Helper()
		rec := b.serve(t, http.MethodGet, "/api/me", "", withSession(ana))
		var me struct {
			Monitors map[string][]string `json:"monitors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &me); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("GET /api/me of ana-lima = %d %s (%v), want 200", rec.Code, shown(t, rec), err)
		}
		return me.Monitors
	}

	replaced := []struct {
		slugs, want []string
		monitors    map[string][]string
	}{
		{[]string{"monitor:sei:view", "monitor:sei:check", "monitor:sei:view", "monitor:*:hook"}, []string{"monitor:*:hook", "monitor:sei:check", "monitor:sei:view"},
			map[string][]string{"diario": {"hook"}, "sei": {"view", "check", "hook"}, "sei-b": {"hook"}}},
		// The database lists sei before sei-b; as slugs, "-" sorts before ":".
		{[]string{"monitor:sei:view", "monitor:sei-b:view"}, []string{"monitor:sei-b:view", "monitor:sei:view"},
			map[string][]string{"sei": {"view"}, "sei-b": {"view"}}},
		{[]string{"monitor:sei:view"}, []string{"monitor:sei:view"}, map[string][]string{"sei": {"view"}}},
		{[]string{}, []string{}, map[string][]string{}},
	}
	for _, r := range replaced {
		rec := b.serve(t, http.MethodPut, "/api/users/ana-lima/permissions", jsonBody(t, map[string]any{"permissions": r.slugs}), withSession(paulo))
		if rec.Code != http.StatusOK {
			t.Fatalf("set the permissions %v = %d %s, want 200", r.slugs, rec.Code, shown(t, rec))
		}
		want := item{Login: "ana-lima", CreatedAt: testNow, Permissions: r.want}
		if got := decodeItem(t, rec); !reflect.DeepEqual(got, want) {
			t.Errorf("ana-lima after setting %v = %+v, want %+v", r.slugs, got, want)
		}
		if got := monitors(); !reflect.DeepEqual(got, r.monitors) {
			t.Errorf("the monitors ana-lima can use after setting %v = %v, want %v", r.slugs, got, r.monitors)
		}
	}

	refused := []struct {
		slug   string
		status int
		detail string
	}{
		{"monitor:nope:view", http.StatusNotFound, "no monitor has this name"},
		{"monitor:sei:read", http.StatusUnprocessableEntity, `not a permission: "monitor:sei:read"`},
	}
	for _, r := range refused {
		body := jsonBody(t, map[string]any{"permissions": []string{"monitor:sei:view", r.slug}})
		rec := b.serve(t, http.MethodPut, "/api/users/ana-lima/permissions", body, withSession(paulo))
		if rec.Code != r.status || detail(t, rec) != r.detail {
			t.Errorf("set the permission %s = %d %s, want %d %q", r.slug, rec.Code, shown(t, rec), r.status, r.detail)
		}
		if got, _ := b.person(t, "ana-lima"); len(got.Permissions) != 0 {
			t.Errorf("ana-lima after a refused %s holds %v, want none", r.slug, got.Permissions)
		}
	}
}

func TestTheLastAdminStays(t *testing.T) {
	b := newBench(t)
	accountstest.Person(t, b.svc, "ana-lima")
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	before, _ := b.person(t, "paulo-santos")

	changes := []struct{ name, method, body string }{
		{"take admin from", http.MethodPatch, `{"admin":false}`},
		{"disable", http.MethodPatch, `{"disabled":true}`},
		{"keep admin but disable", http.MethodPatch, `{"admin":true,"disabled":true}`},
		{"delete", http.MethodDelete, ""},
	}
	for _, c := range changes {
		rec := b.serve(t, c.method, "/api/users/paulo-santos", c.body, withSession(paulo))
		if rec.Code != http.StatusConflict || detail(t, rec) != "this is the last person with admin" {
			t.Errorf("%s the only admin = %d %s, want 409", c.name, rec.Code, shown(t, rec))
		}
		if after, ok := b.person(t, "paulo-santos"); !ok || !reflect.DeepEqual(after, before) {
			t.Errorf("paulo-santos after a refused %s = %+v (listed: %v), want %+v", c.name, after, ok, before)
		}
	}

	promote := b.serve(t, http.MethodPatch, "/api/users/ana-lima", `{"admin":true}`, withSession(paulo))
	if want := (item{Login: "ana-lima", Admin: true, CreatedAt: testNow, Permissions: []string{}}); promote.Code != http.StatusOK || !reflect.DeepEqual(decodeItem(t, promote), want) {
		t.Errorf("give admin to ana-lima = %d %s, want 200 with %+v", promote.Code, shown(t, promote), want)
	}
	demote := b.serve(t, http.MethodPatch, "/api/users/paulo-santos", `{"admin":false}`, withSession(paulo))
	if want := (item{Login: "paulo-santos", CreatedAt: testNow, Permissions: []string{}}); demote.Code != http.StatusOK || !reflect.DeepEqual(decodeItem(t, demote), want) {
		t.Errorf("take admin from paulo-santos once ana-lima has it = %d %s, want 200 with %+v", demote.Code, shown(t, demote), want)
	}
	if got, want := b.admins(t), []string{"ana-lima"}; !slices.Equal(got, want) {
		t.Errorf("the admins = %v, want %v", got, want)
	}
}

func TestTwoAdminsRemovingEachOtherLeaveOne(t *testing.T) {
	b := newBench(t)
	accountstest.Admin(t, b.svc, "ana-lima")
	sessions := map[string]string{
		"paulo-santos": accountstest.Session(t, b.svc, "paulo-santos"),
		"ana-lima":     accountstest.Session(t, b.svc, "ana-lima"),
	}
	targets := map[string]string{"paulo-santos": "ana-lima", "ana-lima": "paulo-santos"}

	// The test holds the access lock, so both changes pass the gate as admins and queue behind it.
	tx, err := b.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if err := db.New(tx).LockAccess(t.Context()); err != nil {
		t.Fatal(err)
	}

	answers := make(chan *httptest.ResponseRecorder, len(targets))
	var wg sync.WaitGroup
	for by, target := range targets {
		wg.Go(func() {
			answers <- serve(t.Context(), b.mux, http.MethodPatch, "/api/users/"+target, `{"admin":false}`, withSession(sessions[by]))
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	coretest.WaitForAdvisoryWaiters(t, b.pool, len(targets), done)
	select {
	case <-done:
		t.Fatal("both changes were answered without queuing on the access lock")
	default:
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-done

	codes := map[int]int{}
	for range targets {
		rec := <-answers
		codes[rec.Code]++
		if rec.Code == http.StatusConflict && detail(t, rec) != "this is the last person with admin" {
			t.Errorf("the refused removal = 409 %s, want the detail %q", shown(t, rec), "this is the last person with admin")
		}
	}
	if want := map[int]int{http.StatusOK: 1, http.StatusConflict: 1}; !maps.Equal(codes, want) {
		t.Errorf("two admins removing each other got the statuses %v, want one 200 and one 409", codes)
	}
	if got := b.admins(t); len(got) != 1 {
		t.Errorf("the admins after two admins removed each other = %v, want exactly one", got)
	}
}

func TestSettingAPasswordEndsTheirSessions(t *testing.T) {
	b := newBench(t)
	accountstest.Person(t, b.svc, "ana-lima")
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	ana := accountstest.Session(t, b.svc, "ana-lima")

	rec := b.serve(t, http.MethodPut, "/api/users/ana-lima/password", jsonBody(t, map[string]any{"password": newPassword}), withSession(paulo))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("set the password of ana-lima = %d %s, want 204 with no body", rec.Code, shown(t, rec))
	}
	if rec := b.serve(t, http.MethodGet, "/api/me", "", withSession(ana)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the session ana-lima opened before the new password = %d %s, want 401", rec.Code, shown(t, rec))
	}
	if rec := b.serve(t, http.MethodGet, "/api/me", "", withSession(paulo)); rec.Code != http.StatusOK {
		t.Errorf("the session of the admin who set the password = %d %s, want 200", rec.Code, shown(t, rec))
	}
	if rec := b.signIn(t, "ana-lima", accountstest.Password); rec.Code != http.StatusUnauthorized {
		t.Errorf("ana-lima signing in with the old password = %d %s, want 401", rec.Code, shown(t, rec))
	}
	if rec := b.signIn(t, "ana-lima", newPassword); rec.Code != http.StatusOK {
		t.Errorf("ana-lima signing in with the new password = %d %s, want 200", rec.Code, shown(t, rec))
	}
}

func TestThePasswordNeverComesBack(t *testing.T) {
	b := newBench(t)
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	create := func(login, password string) string {
		return jsonBody(t, map[string]any{"login": login, "password": password})
	}
	set := func(password string) string { return jsonBody(t, map[string]any{"password": password}) }

	requests := []struct {
		name, method, path, body string
		status                   int
		detail                   string
	}{
		{"create", http.MethodPost, "/api/users", create("ana-lima", anaPassword), http.StatusCreated, ""},
		{"create a login in use", http.MethodPost, "/api/users", create("ana-lima", newPassword), http.StatusConflict, accounts.ErrUserExists.Error()},
		{"create an invalid login", http.MethodPost, "/api/users", create("Ana Lima", newPassword), http.StatusUnprocessableEntity, accounts.ErrInvalidLogin.Error()},
		{"create with a short password", http.MethodPost, "/api/users", create("joana-reis", weakPassword), http.StatusUnprocessableEntity, accounts.ErrWeakPassword.Error()},
		{"create with a long password", http.MethodPost, "/api/users", create("joana-reis", longPassword), http.StatusUnprocessableEntity, accounts.ErrWeakPassword.Error()},
		{"create without a login", http.MethodPost, "/api/users", jsonBody(t, map[string]any{"password": newPassword, "admin": "yes"}), http.StatusUnprocessableEntity, "validation failed"},
		{"set", http.MethodPut, "/api/users/ana-lima/password", set(newPassword), http.StatusNoContent, ""},
		{"set a short password", http.MethodPut, "/api/users/ana-lima/password", set(weakPassword), http.StatusUnprocessableEntity, accounts.ErrWeakPassword.Error()},
		{"set for nobody", http.MethodPut, "/api/users/joana-reis/password", set(newPassword), http.StatusNotFound, accounts.ErrUnknownUser.Error()},
		{"list", http.MethodGet, "/api/users", "", http.StatusOK, ""},
	}
	for _, r := range requests {
		rec := b.serve(t, r.method, r.path, r.body, withSession(paulo))
		for i, password := range passwords {
			if strings.Contains(rec.Body.String(), password) {
				t.Errorf("the response to %s carries test password %d", r.name, i)
			}
		}
		if rec.Code != r.status || (r.detail != "" && detail(t, rec) != r.detail) {
			t.Errorf("%s = %d %s, want %d %q", r.name, rec.Code, shown(t, rec), r.status, r.detail)
		}
	}
	for i, password := range passwords {
		if strings.Contains(b.logs.String(), password) {
			t.Errorf("the logs carry test password %d", i)
		}
	}
}

// leaving stands for a client that goes away while the hub works on its request.
type leaving struct {
	leave func()
}

func (l *leaving) gone(ctx context.Context) error {
	l.leave()
	return ctx.Err()
}

func (l *leaving) ListUsers(ctx context.Context) ([]accounts.User, error) {
	return nil, l.gone(ctx)
}

func (l *leaving) CreateUser(ctx context.Context, _, _ string, _ bool) error {
	return l.gone(ctx)
}

func (l *leaving) UpdateUser(ctx context.Context, _ string, _ accounts.UserChange) error {
	return l.gone(ctx)
}

func (l *leaving) SetPassword(ctx context.Context, _, _ string) error {
	return l.gone(ctx)
}

func (l *leaving) SetPermissions(ctx context.Context, _ string, _ []access.Permission) error {
	return l.gone(ctx)
}

func (l *leaving) DeleteUser(ctx context.Context, _ string) error {
	return l.gone(ctx)
}

// withAccounts serves the people routes over client, with the real gate and
// a session of the admin paulo-santos, whose id it returns.
func withAccounts(t *testing.T, client people.Accounts, logs *bytes.Buffer) (*http.ServeMux, string) {
	t.Helper()
	svc := accounts.NewService(coretest.Pool(t), func() time.Time { return testNow })
	accountstest.Admin(t, svc, "paulo-santos")
	mux := http.NewServeMux()
	humaAPI := humago.New(mux, api.HumaConfig(slog.New(slog.NewTextHandler(logs, nil)), "test", "test"))
	humaAPI.UseMiddleware(gate.Middleware(humaAPI, svc, ""))
	people.RegisterRoutes(humaAPI, people.NewHandler(client))
	return mux, accountstest.Session(t, svc, "paulo-santos")
}

func TestAClientThatLeavesIsNotAServerError(t *testing.T) {
	var logs bytes.Buffer
	client := &leaving{}
	mux, paulo := withAccounts(t, client, &logs)

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/users", ""},
		{http.MethodPost, "/api/users", jsonBody(t, map[string]any{"login": "ana-lima", "password": anaPassword})},
		{http.MethodPatch, "/api/users/ana-lima", `{"disabled":true}`},
		{http.MethodPut, "/api/users/ana-lima/password", jsonBody(t, map[string]any{"password": newPassword})},
		{http.MethodPut, "/api/users/ana-lima/permissions", `{"permissions":["monitor:sei:view"]}`},
		{http.MethodDelete, "/api/users/ana-lima", ""},
	}
	for _, r := range routes {
		ctx, cancel := context.WithCancel(t.Context())
		client.leave = cancel
		rec := serve(ctx, mux, r.method, r.path, r.body, withSession(paulo))
		if rec.Code != 499 {
			t.Errorf("%s %s for a client that left = %d %s, want 499", r.method, r.path, rec.Code, shown(t, rec))
		}
	}
	if strings.Contains(logs.String(), "API request failed") {
		t.Errorf("a client that left was logged as a failure:\n%s", redacted(logs.String()))
	}
}
