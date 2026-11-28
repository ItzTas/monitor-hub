package tokens_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/accounts/accountstest"
	"github.com/ItzTas/monitor-hub/src/internal/api"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/api/session"
	"github.com/ItzTas/monitor-hub/src/internal/api/tokens"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

var testNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// kind is what the tests need to know about tokens or credentials.
type kind struct {
	path    string
	secret  string // the field of the create response that holds the secret
	pattern *regexp.Regexp
	fields  []string // the fields of a listed item, sorted
	unknown string
	body    func(name string, expiresAt *time.Time, slugs ...string) string
	holds   func(t *testing.T, svc *accounts.Service, name, secret string) error
}

var listFields = []string{"created_at", "expires_at", "id", "label", "last_used_at", "owner", "permissions"}

var kinds = map[string]kind{
	"token": {
		path:    "/api/tokens",
		secret:  "token",
		pattern: regexp.MustCompile(`^mht_[A-Za-z0-9_-]{43}$`),
		fields:  listFields,
		unknown: accounts.ErrUnknownToken.Error(),
		body: func(name string, expiresAt *time.Time, slugs ...string) string {
			return createBody(map[string]any{"label": name}, expiresAt, slugs)
		},
		holds: func(t *testing.T, svc *accounts.Service, _, secret string) error {
			_, err := svc.ByToken(t.Context(), secret)
			return err
		},
	},
	"credential": {
		path:    "/api/credentials",
		secret:  "password",
		pattern: regexp.MustCompile(`^mhk_[A-Za-z0-9_-]{43}$`),
		fields:  slices.Sorted(slices.Values(append(slices.Clone(listFields), "username"))),
		unknown: accounts.ErrUnknownCredential.Error(),
		body: func(name string, expiresAt *time.Time, slugs ...string) string {
			return createBody(map[string]any{"label": name, "username": name}, expiresAt, slugs)
		},
		holds: func(t *testing.T, svc *accounts.Service, name, secret string) error {
			_, err := svc.ByCredential(t.Context(), name, secret)
			return err
		},
	},
}

func createBody(fields map[string]any, expiresAt *time.Time, slugs []string) string {
	fields["permissions"] = append([]string{}, slugs...)
	if expiresAt != nil {
		fields["expires_at"] = expiresAt
	}
	body, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(body)
}

type item struct {
	ID          int64      `json:"id"`
	Owner       string     `json:"owner"`
	Label       string     `json:"label"`
	Username    string     `json:"username"`
	Permissions []string   `json:"permissions"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}

type bench struct {
	mux  *http.ServeMux
	pool *pgxpool.Pool
	svc  *accounts.Service
	logs *bytes.Buffer
}

func newBench(t *testing.T) *bench {
	t.Helper()
	pool := coretest.Pool(t)
	b := &bench{
		mux:  http.NewServeMux(),
		pool: pool,
		svc:  accounts.NewService(pool, func() time.Time { return testNow }),
		logs: &bytes.Buffer{},
	}
	accountstest.Person(t, b.svc, "paulo-santos", "monitor:sei:view", "monitor:sei:check", "monitor:sei:hook", "monitor:*:hook")
	accountstest.Person(t, b.svc, "ana-lima", "monitor:sei:hook")
	accountstest.Admin(t, b.svc, "admin")
	api.RegisterAccess(b.mux, api.AccessDeps{Accounts: b.svc, Log: slog.New(slog.NewTextHandler(b.logs, nil)), Logins: session.NewLogins(time.After, nil)})
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

var hidden = regexp.MustCompile(`mh[tk]_[A-Za-z0-9_-]+|[0-9a-f]{64}`)

// shown is the body of rec for a failure message, with secrets and hashes replaced.
func shown(rec *httptest.ResponseRecorder) string {
	return redacted(rec.Body.String())
}

func redacted(text string) string {
	return hidden.ReplaceAllString(text, "[hidden]")
}

// create makes a token or credential named name over the API and returns its id and secret.
func (b *bench) create(t *testing.T, k kind, session, name string, slugs ...string) (int64, string) {
	t.Helper()
	rec := b.serve(t, http.MethodPost, k.path, k.body(name, nil, slugs...), withSession(session))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s at %s = %d %s, want 201", name, k.path, rec.Code, shown(rec))
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the created %s: %v", name, err)
	}
	id, _ := created["id"].(float64)
	secret, _ := created[k.secret].(string)
	return int64(id), secret
}

func (b *bench) list(t *testing.T, k kind, session, query string) []item {
	t.Helper()
	rec := b.serve(t, http.MethodGet, k.path+query, "", withSession(session))
	var page struct {
		Items []item `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("GET %s%s = %d %s (%v), want 200", k.path, query, rec.Code, shown(rec), err)
	}
	return page.Items
}

func owners(items []item) []string {
	out := []string{}
	for _, i := range items {
		out = append(out, i.Owner)
	}
	return out
}

func keys(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode an item: %v", err)
	}
	return slices.Sorted(maps.Keys(m))
}

func detail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var model struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &model); err != nil {
		t.Fatalf("decode the error %s: %v", shown(rec), err)
	}
	return model.Detail
}

func TestCreateShowsTheSecretOnce(t *testing.T) {
	b := newBench(t)
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	expires := testNow.Add(24 * time.Hour)

	for name, k := range kinds {
		rec := b.serve(t, http.MethodPost, k.path, k.body("alerts", &expires, "monitor:sei:hook", "monitor:*:hook", "monitor:sei:hook"), withSession(paulo))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create a %s = %d %s, want 201", name, rec.Code, shown(rec))
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		var secret string
		if err := json.Unmarshal(raw[k.secret], &secret); err != nil || !k.pattern.MatchString(secret) {
			t.Errorf("the %s field of a new %s does not match %s", k.secret, name, k.pattern)
		}
		if got, want := keys(t, rec.Body.Bytes()), slices.Sorted(slices.Values(append(slices.Clone(k.fields), k.secret))); !slices.Equal(got, want) {
			t.Errorf("a new %s has the fields %v, want %v", name, got, want)
		}
		var created item
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}

		listed := b.serve(t, http.MethodGet, k.path, "", withSession(paulo))
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(listed.Body.Bytes(), &page); listed.Code != http.StatusOK || err != nil || len(page.Items) != 1 {
			t.Fatalf("GET %s = %d %s (%v), want 200 with one item", k.path, listed.Code, shown(listed), err)
		}
		if got := keys(t, page.Items[0]); !slices.Equal(got, k.fields) {
			t.Errorf("a listed %s has the fields %v, want %v", name, got, k.fields)
		}
		var first item
		if err := json.Unmarshal(page.Items[0], &first); err != nil {
			t.Fatal(err)
		}

		want := item{
			ID: created.ID, Owner: "paulo-santos", Label: "alerts",
			Permissions: []string{"monitor:*:hook", "monitor:sei:hook"}, CreatedAt: testNow, ExpiresAt: &expires,
		}
		if name == "credential" {
			want.Username = "alerts"
		}
		for shape, got := range map[string]item{"created": created, "listed": first} {
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the %s %s = %+v, want %+v", shape, name, got, want)
			}
		}
	}
}

// TestTimesAreInUTC runs in the zone TestMain sets, which is not UTC.
func TestTimesAreInUTC(t *testing.T) {
	b := newBench(t)
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	expires := testNow.Add(24 * time.Hour).In(time.Local)

	for name, k := range kinds {
		created := b.serve(t, http.MethodPost, k.path, k.body("zones", &expires, "monitor:sei:hook"), withSession(paulo))
		var made map[string]any
		if err := json.Unmarshal(created.Body.Bytes(), &made); created.Code != http.StatusCreated || err != nil {
			t.Fatalf("create a %s = %d %s (%v), want 201", name, created.Code, shown(created), err)
		}
		secret, _ := made[k.secret].(string)
		if err := k.holds(t, b.svc, "zones", secret); err != nil {
			t.Fatalf("use the new %s: %v", name, err)
		}
		listed := b.serve(t, http.MethodGet, k.path, "", withSession(paulo))
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(listed.Body.Bytes(), &page); listed.Code != http.StatusOK || err != nil || len(page.Items) != 1 {
			t.Fatalf("GET %s = %d %s (%v), want 200 with one item", k.path, listed.Code, shown(listed), err)
		}

		shapes := map[string]struct {
			item   map[string]any
			fields []string
		}{
			"created": {made, []string{"created_at", "expires_at"}},
			"listed":  {page.Items[0], []string{"created_at", "expires_at", "last_used_at"}},
		}
		for shape, s := range shapes {
			for _, field := range s.fields {
				if at, _ := s.item[field].(string); !strings.HasSuffix(at, "Z") {
					t.Errorf("the %s %s has %s %q, want a time in UTC", shape, name, field, at)
				}
			}
		}
	}
}

func TestListsNeverShowASecret(t *testing.T) {
	b := newBench(t)
	sessions := map[string]string{}
	for _, login := range []string{"paulo-santos", "ana-lima", "admin"} {
		sessions[login] = accountstest.Session(t, b.svc, login)
	}
	secrets := []string{}
	for _, k := range kinds {
		for _, login := range []string{"paulo-santos", "ana-lima"} {
			_, secret := b.create(t, k, sessions[login], login+"-alerts", "monitor:sei:hook")
			secrets = append(secrets, secret)
		}
	}

	views := map[string]string{"paulo-santos": "", "ana-lima": "", "admin": "?everyone=true"}
	for name, k := range kinds {
		for login, query := range views {
			rec := b.serve(t, http.MethodGet, k.path+query, "", withSession(sessions[login]))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"owner"`) {
				t.Errorf("GET %s%s by %s = %d %s, want 200 with items", k.path, query, login, rec.Code, shown(rec))
			}
			for i, secret := range secrets {
				for form, value := range map[string]string{"in full": secret, "without its prefix": secret[4:], "as its hash": accounts.HashSecret(secret)} {
					if strings.Contains(rec.Body.String(), value) {
						t.Errorf("the %s list of %s carries secret %d %s", name, login, i, form)
					}
				}
			}
		}
	}
	for i, secret := range secrets {
		if strings.Contains(b.logs.String(), secret[4:]) {
			t.Errorf("the logs carry secret %d", i)
		}
	}
}

func TestEveryoneNeedsAnAdmin(t *testing.T) {
	b := newBench(t)
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	ana := accountstest.Session(t, b.svc, "ana-lima")
	admin := accountstest.Session(t, b.svc, "admin")

	for name, k := range kinds {
		b.create(t, k, paulo, "paulo-alerts", "monitor:sei:hook")
		b.create(t, k, ana, "ana-alerts", "monitor:sei:hook")

		rec := b.serve(t, http.MethodGet, k.path+"?everyone=true", "", withSession(paulo))
		if rec.Code != http.StatusForbidden || detail(t, rec) != "you do not have permission for this" {
			t.Errorf("everyone's %ss for paulo-santos = %d %s, want 403", name, rec.Code, shown(rec))
		}
		if got, want := owners(b.list(t, k, paulo, "")), []string{"paulo-santos"}; !slices.Equal(got, want) {
			t.Errorf("the %ss of paulo-santos belong to %v, want %v", name, got, want)
		}
		if got, want := owners(b.list(t, k, admin, "?everyone=true")), []string{"paulo-santos", "ana-lima"}; !slices.Equal(got, want) {
			t.Errorf("everyone's %ss for an admin belong to %v, want %v", name, got, want)
		}
		own := b.serve(t, http.MethodGet, k.path, "", withSession(admin))
		if got := strings.TrimSpace(own.Body.String()); own.Code != http.StatusOK || got != `{"items":[]}` {
			t.Errorf("the own %ss of an admin who has none = %d %s, want 200 {\"items\":[]}", name, own.Code, shown(own))
		}
	}
}

func TestRevokeOnlyYourOwn(t *testing.T) {
	b := newBench(t)
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	ana := accountstest.Session(t, b.svc, "ana-lima")
	admin := accountstest.Session(t, b.svc, "admin")

	for name, k := range kinds {
		first, firstSecret := b.create(t, k, paulo, "paulo-first", "monitor:sei:hook")
		second, secondSecret := b.create(t, k, paulo, "paulo-second", "monitor:sei:hook")
		target := func(id int64) string { return fmt.Sprintf("%s/%d", k.path, id) }

		rec := b.serve(t, http.MethodDelete, target(first), "", withSession(ana))
		if rec.Code != http.StatusNotFound || detail(t, rec) != k.unknown {
			t.Errorf("ana-lima revoking a %s of paulo-santos = %d %s, want 404 %q", name, rec.Code, shown(rec), k.unknown)
		}
		if err := k.holds(t, b.svc, "paulo-first", firstSecret); err != nil {
			t.Errorf("the %s ana-lima tried to revoke stopped working: %v", name, err)
		}

		revokes := []struct {
			by, session string
			id          int64
			name        string
			secret      string
		}{
			{"paulo-santos", paulo, second, "paulo-second", secondSecret},
			{"an admin", admin, first, "paulo-first", firstSecret},
		}
		for _, r := range revokes {
			rec := b.serve(t, http.MethodDelete, target(r.id), "", withSession(r.session))
			if rec.Code != http.StatusNoContent {
				t.Errorf("%s revoking the %s %s = %d %s, want 204", r.by, name, r.name, rec.Code, shown(rec))
			}
			if err := k.holds(t, b.svc, r.name, r.secret); !errors.Is(err, accounts.ErrUnauthenticated) {
				t.Errorf("the %s %s after %s revoked it: err = %v, want ErrUnauthenticated", name, r.name, r.by, err)
			}
		}
	}
}

func TestTokensCannotManageTokens(t *testing.T) {
	b := newBench(t)
	token := accountstest.Token(t, b.svc, "paulo-santos", "monitor:sei:hook")

	for name, k := range kinds {
		routes := map[string]struct{ path, body string }{
			http.MethodGet:    {k.path, ""},
			http.MethodPost:   {k.path, k.body("from-a-token", nil, "monitor:sei:hook")},
			http.MethodDelete: {k.path + "/1", ""},
		}
		for method, r := range routes {
			rec := b.serve(t, method, r.path, r.body, withBearer(token))
			if rec.Code != http.StatusForbidden || detail(t, rec) != "this route needs a signed-in session" {
				t.Errorf("%s %s (%ss) with a token = %d %s, want 403", method, r.path, name, rec.Code, shown(rec))
			}
		}
	}
}

func TestRefusalsHaveTheirStatus(t *testing.T) {
	b := newBench(t)
	sessions := map[string]string{
		"paulo-santos": accountstest.Session(t, b.svc, "paulo-santos"),
		"admin":        accountstest.Session(t, b.svc, "admin"),
	}
	b.create(t, kinds["credential"], sessions["paulo-santos"], "taken-name", "monitor:sei:hook")
	now, err := json.Marshal(testNow)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		login, method, path, body string
		status                    int
		detail                    string
	}{
		"a slug that is not a permission": {
			"paulo-santos", http.MethodPost, "/api/tokens", `{"label":"rss","permissions":["monitor:sei"]}`,
			http.StatusUnprocessableEntity, `not a permission: "monitor:sei"`,
		},
		"an empty label": {
			"paulo-santos", http.MethodPost, "/api/tokens", `{"label":"","permissions":["monitor:sei:view"]}`,
			http.StatusUnprocessableEntity, accounts.ErrInvalidLabel.Error(),
		},
		"an expiry now": {
			"paulo-santos", http.MethodPost, "/api/tokens", `{"label":"rss","permissions":["monitor:sei:view"],"expires_at":` + string(now) + `}`,
			http.StatusUnprocessableEntity, accounts.ErrPastExpiry.Error(),
		},
		"configure in a token": {
			"paulo-santos", http.MethodPost, "/api/tokens", `{"label":"rss","permissions":["monitor:sei:configure"]}`,
			http.StatusUnprocessableEntity, accounts.ErrTokenCarries.Error(),
		},
		"view in a credential": {
			"paulo-santos", http.MethodPost, "/api/credentials", `{"label":"alerts","username":"alerts","permissions":["monitor:sei:view"]}`,
			http.StatusUnprocessableEntity, accounts.ErrCredentialCarries.Error(),
		},
		"an invalid username": {
			"paulo-santos", http.MethodPost, "/api/credentials", `{"label":"alerts","username":"Alerts","permissions":["monitor:sei:hook"]}`,
			http.StatusUnprocessableEntity, accounts.ErrInvalidUsername.Error(),
		},
		"no permissions": {
			"paulo-santos", http.MethodPost, "/api/tokens", `{"label":"rss","permissions":[]}`,
			http.StatusUnprocessableEntity, accounts.ErrNoPermissions.Error(),
		},
		"a permission its owner does not have": {
			"paulo-santos", http.MethodPost, "/api/tokens", `{"label":"rss","permissions":["monitor:diario:view"]}`,
			http.StatusForbidden, "you do not have monitor:diario:view",
		},
		"a username in use": {
			"paulo-santos", http.MethodPost, "/api/credentials", `{"label":"alerts","username":"taken-name","permissions":["monitor:sei:hook"]}`,
			http.StatusConflict, accounts.ErrUsernameTaken.Error(),
		},
		"a monitor that does not exist": {
			"admin", http.MethodPost, "/api/tokens", `{"label":"rss","permissions":["monitor:nope:view"]}`,
			http.StatusNotFound, accounts.ErrUnknownMonitor.Error(),
		},
		"an unknown token": {
			"paulo-santos", http.MethodDelete, "/api/tokens/999999", "",
			http.StatusNotFound, accounts.ErrUnknownToken.Error(),
		},
		"an unknown credential": {
			"paulo-santos", http.MethodDelete, "/api/credentials/999999", "",
			http.StatusNotFound, accounts.ErrUnknownCredential.Error(),
		},
	}
	for name, c := range cases {
		rec := b.serve(t, c.method, c.path, c.body, withSession(sessions[c.login]))
		if rec.Code != c.status || detail(t, rec) != c.detail {
			t.Errorf("%s: %s %s = %d %s, want %d %q", name, c.method, c.path, rec.Code, shown(rec), c.status, c.detail)
		}
	}
}

func TestAMonitorGoneLeavesAnEmptyPermissionList(t *testing.T) {
	b := newBench(t)
	paulo := accountstest.Session(t, b.svc, "paulo-santos")
	for _, k := range kinds {
		b.create(t, k, paulo, "alerts", "monitor:sei:hook")
	}
	if _, err := b.pool.Exec(t.Context(), "DELETE FROM monitors WHERE name = 'sei'"); err != nil {
		t.Fatal(err)
	}

	for name, k := range kinds {
		rec := b.serve(t, http.MethodGet, k.path, "", withSession(paulo))
		var page struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); rec.Code != http.StatusOK || err != nil || len(page.Items) != 1 {
			t.Fatalf("GET %s = %d %s (%v), want 200 with one item", k.path, rec.Code, shown(rec), err)
		}
		if got := string(page.Items[0]["permissions"]); got != "[]" {
			t.Errorf("a %s whose monitor is gone lists the permissions %s, want []", name, got)
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

func (l *leaving) CreateToken(ctx context.Context, _ access.Principal, _ accounts.TokenRequest) (accounts.Token, string, error) {
	return accounts.Token{}, "", l.gone(ctx)
}

func (l *leaving) Tokens(ctx context.Context, _ int64) ([]accounts.Token, error) {
	return nil, l.gone(ctx)
}

func (l *leaving) RevokeToken(ctx context.Context, _, _ int64) error {
	return l.gone(ctx)
}

func (l *leaving) CreateCredential(ctx context.Context, _ access.Principal, _ string, _ accounts.TokenRequest) (accounts.Credential, string, error) {
	return accounts.Credential{}, "", l.gone(ctx)
}

func (l *leaving) Credentials(ctx context.Context, _ int64) ([]accounts.Credential, error) {
	return nil, l.gone(ctx)
}

func (l *leaving) RevokeCredential(ctx context.Context, _, _ int64) error {
	return l.gone(ctx)
}

func TestAClientThatLeavesIsNotAServerError(t *testing.T) {
	svc := accounts.NewService(coretest.Pool(t), func() time.Time { return testNow })
	accountstest.Person(t, svc, "paulo-santos", "monitor:sei:hook")
	paulo := accountstest.Session(t, svc, "paulo-santos")
	var logs bytes.Buffer
	mux := http.NewServeMux()
	humaAPI := humago.New(mux, api.HumaConfig(slog.New(slog.NewTextHandler(&logs, nil)), "test", "test"))
	humaAPI.UseMiddleware(gate.Middleware(humaAPI, svc, ""))
	client := &leaving{}
	tokens.RegisterRoutes(humaAPI, tokens.NewHandler(client))

	for _, k := range kinds {
		routes := map[string]struct{ path, body string }{
			http.MethodGet:    {k.path, ""},
			http.MethodPost:   {k.path, k.body("alerts", nil, "monitor:sei:hook")},
			http.MethodDelete: {k.path + "/1", ""},
		}
		for method, r := range routes {
			ctx, cancel := context.WithCancel(t.Context())
			client.leave = cancel
			rec := serve(ctx, mux, method, r.path, r.body, withSession(paulo))
			if rec.Code != 499 {
				t.Errorf("%s %s for a client that left = %d %s, want 499", method, r.path, rec.Code, shown(rec))
			}
		}
	}
	if strings.Contains(logs.String(), "API request failed") {
		t.Errorf("a client that left was logged as a failure:\n%s", redacted(logs.String()))
	}
}
