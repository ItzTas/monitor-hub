package gate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/accounts/accountstest"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

const publicURL = "https://hub.example"

var testNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

type caller struct {
	Kind  access.Kind `json:"kind"`
	Login string      `json:"login"`
}

type callerOutput struct {
	Body caller
}

func answer(ctx context.Context, _ *struct{}) (*callerOutput, error) {
	p := access.FromContext(ctx)
	return &callerOutput{Body: caller{Kind: p.Kind(), Login: p.Login()}}, nil
}

func onlyAdmins(ctx context.Context, input *struct{}) (*callerOutput, error) {
	if err := NeedAdmin(ctx); err != nil {
		return nil, err
	}
	return answer(ctx, input)
}

// newMux serves /anyone, /identified and /session at their level for every
// method, /unmarked without a level and /admin behind NeedAdmin.
func newMux(resolver Resolver) *http.ServeMux {
	mux := http.NewServeMux()
	config := huma.DefaultConfig("test", "1.0.0")
	config.CreateHooks = nil
	api := humago.New(mux, config)
	api.UseMiddleware(Middleware(api, resolver, publicURL))
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	for path, level := range map[string]Level{"/anyone": Anyone, "/identified": Identified, "/session": Session} {
		for _, method := range methods {
			huma.Register(api, huma.Operation{OperationID: method + path, Method: method, Path: path, Metadata: Meta(level)}, answer)
		}
	}
	huma.Register(api, huma.Operation{OperationID: "unmarked", Method: http.MethodGet, Path: "/unmarked"}, answer)
	huma.Register(api, huma.Operation{OperationID: "admin", Method: http.MethodGet, Path: "/admin", Metadata: Meta(Session)}, onlyAdmins)
	return mux
}

type edit func(*http.Request)

func bearer(secret string) edit {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func cookie(id string) edit {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: id}) }
}

func origin(o string) edit {
	return func(r *http.Request) { r.Header.Set("Origin", o) }
}

func serve(t *testing.T, mux *http.ServeMux, method, path string, edits ...edit) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	for _, e := range edits {
		e(request)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, request)
	return rec
}

func decodeCaller(t *testing.T, rec *httptest.ResponseRecorder) caller {
	t.Helper()
	var got caller
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return got
}

func detail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body.Detail
}

// clearedCookie reports whether the response clears the session cookie.
func clearedCookie(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	for _, line := range rec.Header().Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err != nil {
			t.Fatalf("parse Set-Cookie: %v", err)
		}
		if c.Name == CookieName && c.Value == "" && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

type fixture struct {
	svc          *accounts.Service
	later        *accounts.Service // 13 hours after svc: sessions of svc and short tokens have expired
	session      string
	token        string
	adminSession string
	adminToken   string
	shortToken   string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := coretest.Pool(t)
	svc := accounts.NewService(pool, func() time.Time { return testNow })
	accountstest.Person(t, svc, "ana-lima", "monitor:*:view")
	accountstest.Admin(t, svc, "paulo-santos")
	_, owner, err := svc.Login(t.Context(), "ana-lima", accountstest.Password)
	if err != nil {
		t.Fatal(err)
	}
	expires := testNow.Add(time.Hour)
	view, err := access.Parse("monitor:*:view")
	if err != nil {
		t.Fatal(err)
	}
	_, short, err := svc.CreateToken(t.Context(), owner, accounts.TokenRequest{Label: "short", Permissions: []access.Permission{view}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		svc:          svc,
		later:        accounts.NewService(pool, func() time.Time { return testNow.Add(13 * time.Hour) }),
		session:      accountstest.Session(t, svc, "ana-lima"),
		token:        accountstest.Token(t, svc, "ana-lima", "monitor:*:view"),
		adminSession: accountstest.Session(t, svc, "paulo-santos"),
		adminToken:   accountstest.Token(t, svc, "paulo-santos", "monitor:*:view"),
		shortToken:   short,
	}
}

func TestEachLevelLetsInWhoItShould(t *testing.T) {
	f := newFixture(t)
	mux := newMux(f.svc)
	callers := map[string][]edit{
		"nobody":              nil,
		"a session":           {cookie(f.session)},
		"a token":             {bearer(f.token)},
		"an admin session":    {cookie(f.adminSession)},
		"the token of admins": {bearer(f.adminToken)},
	}
	refusals := map[int]string{
		http.StatusUnauthorized: "sign in or send a personal token",
		http.StatusForbidden:    "this route needs a signed-in session",
	}
	tests := []struct {
		path string
		who  string
		want int
		kind access.Kind
	}{
		{"/anyone", "nobody", http.StatusOK, access.Nobody},
		{"/anyone", "a session", http.StatusOK, access.Session},
		{"/anyone", "a token", http.StatusOK, access.Token},
		{"/identified", "nobody", http.StatusUnauthorized, 0},
		{"/identified", "a session", http.StatusOK, access.Session},
		{"/identified", "a token", http.StatusOK, access.Token},
		{"/session", "nobody", http.StatusUnauthorized, 0},
		{"/session", "a session", http.StatusOK, access.Session},
		{"/session", "a token", http.StatusForbidden, 0},
		{"/unmarked", "nobody", http.StatusUnauthorized, 0},
		{"/unmarked", "a session", http.StatusOK, access.Session},
		{"/unmarked", "a token", http.StatusForbidden, 0},
	}
	for _, tt := range tests {
		rec := serve(t, mux, http.MethodGet, tt.path, callers[tt.who]...)
		if rec.Code != tt.want {
			t.Errorf("GET %s by %s = %d %s, want %d", tt.path, tt.who, rec.Code, rec.Body.String(), tt.want)
			continue
		}
		if tt.want != http.StatusOK {
			if got := detail(t, rec); got != refusals[tt.want] {
				t.Errorf("GET %s by %s says %q, want %q", tt.path, tt.who, got, refusals[tt.want])
			}
			if challenge := rec.Header().Get("WWW-Authenticate"); (tt.want == http.StatusUnauthorized) != (challenge == "Bearer") {
				t.Errorf("GET %s by %s has WWW-Authenticate %q", tt.path, tt.who, challenge)
			}
			continue
		}
		got := decodeCaller(t, rec)
		wantLogin := map[access.Kind]string{access.Nobody: "", access.Session: "ana-lima", access.Token: "ana-lima"}[tt.kind]
		if got.Kind != tt.kind || got.Login != wantLogin {
			t.Errorf("GET %s by %s reached the route as %+v, want kind %v of %q", tt.path, tt.who, got, tt.kind, wantLogin)
		}
	}

	admin := map[string]int{
		"nobody":              http.StatusUnauthorized,
		"a session":           http.StatusForbidden,
		"the token of admins": http.StatusForbidden,
		"an admin session":    http.StatusOK,
	}
	for who, want := range admin {
		rec := serve(t, mux, http.MethodGet, "/admin", callers[who]...)
		if rec.Code != want {
			t.Errorf("GET /admin by %s = %d %s, want %d", who, rec.Code, rec.Body.String(), want)
		}
	}
	if rec := serve(t, mux, http.MethodGet, "/admin", callers["a session"]...); detail(t, rec) != "you do not have permission for this" {
		t.Errorf("GET /admin by a session says %q", detail(t, rec))
	}
}

func TestAnInvalidCredentialIsRefused(t *testing.T) {
	f := newFixture(t)
	now, later := newMux(f.svc), newMux(f.later)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("ana-lima-hook:mhk_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"))
	headers := []struct {
		name string
		mux  *http.ServeMux
		edit edit
	}{
		{"an unknown token", now, bearer("mht_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG")},
		{"an expired token", later, bearer(f.shortToken)},
		{"a session id as a token", now, bearer(f.session)},
		{"Basic", now, func(r *http.Request) { r.Header.Set("Authorization", basic) }},
	}
	for _, path := range []string{"/anyone", "/identified", "/session"} {
		for _, h := range headers {
			rec := serve(t, h.mux, http.MethodGet, path, h.edit)
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("GET %s with %s = %d %s, want 401 with WWW-Authenticate: Bearer", path, h.name, rec.Code, rec.Body.String())
			}
		}
	}

	cookies := []struct {
		name string
		mux  *http.ServeMux
		id   string
	}{
		{"an unknown session", now, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"},
		{"an expired session", later, f.session},
	}
	for _, path := range []string{"/identified", "/session"} {
		for _, c := range cookies {
			rec := serve(t, c.mux, http.MethodGet, path, cookie(c.id))
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("GET %s with %s = %d %s, want 401 with WWW-Authenticate: Bearer", path, c.name, rec.Code, rec.Body.String())
			}
			if !clearedCookie(t, rec) {
				t.Errorf("GET %s with %s does not clear the session cookie", path, c.name)
			}
		}
	}
}

func TestAStaleCookieDoesNotBlockSigningIn(t *testing.T) {
	f := newFixture(t)
	cookies := map[string]struct {
		mux *http.ServeMux
		id  string
	}{
		"an unknown session": {newMux(f.svc), "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"},
		"an expired session": {newMux(f.later), f.session},
	}
	for name, c := range cookies {
		rec := serve(t, c.mux, http.MethodPost, "/anyone", cookie(c.id), origin("http://example.com"))
		if rec.Code != http.StatusOK {
			t.Errorf("POST /anyone with %s = %d %s, want 200", name, rec.Code, rec.Body.String())
			continue
		}
		if got := decodeCaller(t, rec); got.Kind != access.Nobody {
			t.Errorf("POST /anyone with %s reached the route as %+v, want nobody", name, got)
		}
		if !clearedCookie(t, rec) {
			t.Errorf("POST /anyone with %s does not clear the session cookie", name)
		}
	}
}

func TestTheBearerWinsOverTheCookie(t *testing.T) {
	f := newFixture(t)
	mux := newMux(f.svc)

	rec := serve(t, mux, http.MethodGet, "/identified", bearer(f.token), cookie(f.adminSession))
	if got := decodeCaller(t, rec); rec.Code != http.StatusOK || got.Kind != access.Token || got.Login != "ana-lima" {
		t.Errorf("a token with an admin cookie reached /identified as %d %+v, want the token of ana-lima", rec.Code, got)
	}
	if rec := serve(t, mux, http.MethodGet, "/session", bearer(f.token), cookie(f.adminSession)); rec.Code != http.StatusForbidden {
		t.Errorf("a token with a session cookie on /session = %d, want 403", rec.Code)
	}
	if rec := serve(t, mux, http.MethodGet, "/identified", bearer("mht_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"), cookie(f.session)); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unknown token with a session cookie = %d, want 401", rec.Code)
	}
}

func TestASessionChangeFromAnotherOriginIs403(t *testing.T) {
	f := newFixture(t)
	mux := newMux(f.svc)
	const refusal = "the request comes from another site"

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := serve(t, mux, method, "/identified", cookie(f.session))
		if rec.Code != http.StatusForbidden || detail(t, rec) != refusal {
			t.Errorf("%s with a session and no Origin = %d %s, want 403 %q", method, rec.Code, rec.Body.String(), refusal)
		}
	}
	tests := []struct {
		name   string
		method string
		path   string
		edits  []edit
		want   int
	}{
		{"a session from another site", http.MethodPost, "/identified", []edit{cookie(f.session), origin("http://evil.example")}, http.StatusForbidden},
		{"a session from http://<Host>", http.MethodPost, "/identified", []edit{cookie(f.session), origin("http://example.com")}, http.StatusOK},
		{"a session from https://<Host>", http.MethodPost, "/identified", []edit{cookie(f.session), origin("https://example.com")}, http.StatusOK},
		{"a session from PUBLIC_URL", http.MethodPost, "/identified", []edit{cookie(f.session), origin(publicURL)}, http.StatusOK},
		{"a session reading without Origin", http.MethodGet, "/identified", []edit{cookie(f.session)}, http.StatusOK},
		{"a token without Origin", http.MethodPost, "/identified", []edit{bearer(f.token)}, http.StatusOK},
		{"a token from another site", http.MethodPost, "/identified", []edit{bearer(f.token), origin("http://evil.example")}, http.StatusForbidden},
		{"nobody from another site", http.MethodPost, "/anyone", []edit{origin("http://evil.example")}, http.StatusForbidden},
		{"nobody without Origin", http.MethodPost, "/anyone", nil, http.StatusOK},
	}
	for _, tt := range tests {
		rec := serve(t, mux, tt.method, tt.path, tt.edits...)
		if rec.Code != tt.want {
			t.Errorf("%s: %s %s = %d %s, want %d", tt.name, tt.method, tt.path, rec.Code, rec.Body.String(), tt.want)
		}
	}
}

type failingResolver struct{}

func (failingResolver) BySession(context.Context, string) (access.Principal, error) {
	return access.Principal{}, errors.New("connection refused")
}

func (failingResolver) ByToken(context.Context, string) (access.Principal, error) {
	return access.Principal{}, errors.New("connection refused")
}

func TestAResolverFailureIs500(t *testing.T) {
	mux := newMux(failingResolver{})
	for name, e := range map[string]edit{"a cookie": cookie("some-session"), "a token": bearer("mht_some-token")} {
		rec := serve(t, mux, http.MethodGet, "/anyone", e)
		if rec.Code != http.StatusInternalServerError || detail(t, rec) != "internal error" {
			t.Errorf("GET /anyone with %s and a failing database = %d %s, want 500 internal error", name, rec.Code, rec.Body.String())
		}
	}
}

func TestRefuseIs401ForNobodyAnd403Otherwise(t *testing.T) {
	tests := map[string]struct {
		ctx       context.Context
		status    int
		challenge string
	}{
		"nobody":  {t.Context(), http.StatusUnauthorized, "Bearer"},
		"a token": {access.WithPrincipal(t.Context(), access.NewToken(access.Person{Login: "ana-lima"}, nil)), http.StatusForbidden, ""},
	}
	for name, tt := range tests {
		err := Refuse(tt.ctx)
		var status huma.StatusError
		if !errors.As(err, &status) || status.GetStatus() != tt.status {
			t.Errorf("Refuse for %s = %v, want status %d", name, err, tt.status)
		}
		challenge := ""
		var headers huma.HeadersError
		if errors.As(err, &headers) {
			challenge = headers.GetHeaders().Get("WWW-Authenticate")
		}
		if challenge != tt.challenge {
			t.Errorf("Refuse for %s has WWW-Authenticate %q, want %q", name, challenge, tt.challenge)
		}
	}
}
