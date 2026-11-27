package accounts

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/access"
)

func principalOf(t *testing.T, s *Service, name string) access.Principal {
	t.Helper()
	_, p, err := s.Login(t.Context(), name, password)
	if err != nil {
		t.Fatalf("login %s: %v", name, err)
	}
	return p
}

func serviceAt(pool *pgxpool.Pool, d time.Duration) *Service {
	return NewService(pool, func() time.Time { return testNow.Add(d) })
}

func createToken(t *testing.T, s *Service, owner access.Principal, req TokenRequest) (Token, string) {
	t.Helper()
	token, secret, err := s.CreateToken(t.Context(), owner, req)
	if err != nil {
		t.Fatalf("create the token %q: %v", req.Label, err)
	}
	return token, secret
}

func wantUnauthenticated(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("%s: err = %v; want ErrUnauthenticated", name, err)
	}
}

func wantNotHeld(t *testing.T, name string, err error, slug string) {
	t.Helper()
	var notHeld *NotHeldError
	if !errors.As(err, &notHeld) || notHeld.Permission.String() != slug || err.Error() != "you do not have "+slug {
		t.Errorf("%s: err = %v; want a NotHeldError for %s", name, err, slug)
	}
}

func TestATokenCarriesOnlyWhatItsOwnerHas(t *testing.T) {
	s, _ := newService(t)
	prepare(t, s)
	create(t, s, "paulo-santos", false)
	create(t, s, "ana-lima", true)
	setPermissions(t, s, "paulo-santos", "monitor:sei:view", "monitor:sei:check", "monitor:sei:hook")
	paulo := principalOf(t, s, "paulo-santos")
	seiView := permissions(t, "monitor:sei:view")

	token, secret, err := s.CreateToken(t.Context(), paulo, TokenRequest{Label: "rss", Permissions: seiView})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^mht_[A-Za-z0-9_-]{43}$`).MatchString(secret) {
		t.Error("the token is not mht_ followed by 43 base64url characters")
	}
	if strings.Contains(fmt.Sprintf("%+v", token), secret[4:]) {
		t.Fatal("the created Token holds the secret")
	}
	want := Token{ID: token.ID, Owner: "paulo-santos", Label: "rss", Permissions: seiView, CreatedAt: testNow}
	if !reflect.DeepEqual(token, want) {
		t.Errorf("CreateToken = %+v; want %+v", token, want)
	}

	past := testNow
	refused := map[string]struct {
		req  TokenRequest
		want error
		text string
	}{
		"configure": {
			TokenRequest{Label: "rss", Permissions: permissions(t, "monitor:sei:configure")},
			ErrTokenCarries, "a token carries only view, check and hook of monitors",
		},
		"an expiry now": {
			TokenRequest{Label: "rss", Permissions: seiView, ExpiresAt: &past},
			ErrPastExpiry, "expires_at must be in the future",
		},
		"an empty label": {
			TokenRequest{Permissions: seiView},
			ErrInvalidLabel, "use a label of 1 to 64 characters without control characters",
		},
		"a label of 65 characters": {
			TokenRequest{Label: strings.Repeat("a", 65), Permissions: seiView},
			ErrInvalidLabel, "use a label of 1 to 64 characters without control characters",
		},
		"a label with a newline": {
			TokenRequest{Label: "rss\nfeed", Permissions: seiView},
			ErrInvalidLabel, "use a label of 1 to 64 characters without control characters",
		},
		"no permissions": {
			TokenRequest{Label: "rss"},
			ErrNoPermissions, "choose at least one permission",
		},
	}
	for name, c := range refused {
		if _, _, err := s.CreateToken(t.Context(), paulo, c.req); !errors.Is(err, c.want) || err.Error() != c.text {
			t.Errorf("%s: err = %v; want %q", name, err, c.text)
		}
	}
	_, _, err = s.CreateCredential(t.Context(), paulo, "alerts-paulo", TokenRequest{Label: "alerts", Permissions: seiView})
	if !errors.Is(err, ErrCredentialCarries) || err.Error() != "a credential carries only hook of monitors" {
		t.Errorf("a credential with view: err = %v; want ErrCredentialCarries", err)
	}
	for _, slug := range []string{"monitor:diario:view", "monitor:*:view"} {
		_, _, err := s.CreateToken(t.Context(), paulo, TokenRequest{Label: "rss", Permissions: permissions(t, "monitor:sei:check", slug)})
		wantNotHeld(t, slug, err, slug)
	}
	byToken, err := s.ByToken(t.Context(), secret)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.CreateToken(t.Context(), byToken, TokenRequest{Label: "copy", Permissions: seiView})
	wantNotHeld(t, "a token creating a token", err, "monitor:sei:view")

	tokens, err := s.Tokens(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 {
		t.Fatalf("tokens after refused creations = %d; want 1", len(tokens))
	}

	ana := principalOf(t, s, "ana-lima")
	createToken(t, s, ana, TokenRequest{Label: strings.Repeat("é", 64), Permissions: permissions(t, "monitor:*:check")})
	_, _, err = s.CreateToken(t.Context(), ana, TokenRequest{Label: "nope", Permissions: permissions(t, "monitor:nope:check")})
	if !errors.Is(err, ErrUnknownMonitor) {
		t.Errorf("a token on a missing monitor: err = %v; want ErrUnknownMonitor", err)
	}
	_, _, err = s.CreateCredential(t.Context(), ana, "alerts-ana", TokenRequest{Label: "nope", Permissions: permissions(t, "monitor:nope:hook")})
	if !errors.Is(err, ErrUnknownMonitor) {
		t.Errorf("a credential on a missing monitor: err = %v; want ErrUnknownMonitor", err)
	}
}

func TestATokenFollowsItsOwnerOnTheNextRequest(t *testing.T) {
	s, pool := newService(t)
	prepare(t, s)
	create(t, s, "paulo-santos", false)
	setPermissions(t, s, "paulo-santos", "monitor:sei:view", "monitor:diario:view")
	paulo := principalOf(t, s, "paulo-santos")
	seiView := TokenRequest{Label: "rss", Permissions: permissions(t, "monitor:sei:view")}
	_, secret := createToken(t, s, paulo, seiView)
	revoked, revokedSecret := createToken(t, s, paulo, seiView)
	planted, _ := createToken(t, s, paulo, seiView)
	expiry := testNow.Add(time.Hour)
	_, expiring := createToken(t, s, paulo, TokenRequest{Label: "rss", Permissions: seiView.Permissions, ExpiresAt: &expiry})

	canView := func(name string, want bool) {
		t.Helper()
		p, err := s.ByToken(t.Context(), secret)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Kind() != access.Token || p.Login() != "paulo-santos" || p.Can(access.View, "diario") {
			t.Errorf("%s: kind %v %q view diario %v; want a token of paulo-santos without diario",
				name, p.Kind(), p.Login(), p.Can(access.View, "diario"))
		}
		if p.Can(access.View, "sei") != want {
			t.Errorf("%s: view sei = %v; want %v", name, !want, want)
		}
	}
	canView("a token of a person with view on sei", true)
	setPermissions(t, s, "paulo-santos")
	canView("after the owner loses her permissions", false)
	setPermissions(t, s, "paulo-santos", "monitor:sei:view")
	canView("after the owner gets view on sei back", true)

	if err := s.RevokeToken(t.Context(), revoked.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, err := s.ByToken(t.Context(), revokedSecret)
	wantUnauthenticated(t, "a revoked token", err)

	if _, err := serviceAt(pool, time.Hour-time.Second).ByToken(t.Context(), expiring); err != nil {
		t.Errorf("a second before the expiry: %v", err)
	}
	_, err = serviceAt(pool, time.Hour).ByToken(t.Context(), expiring)
	wantUnauthenticated(t, "at the expiry", err)

	consumerLike := "mhc_" + strings.Repeat("a", 43)
	exec(t, pool, "UPDATE tokens SET hash = $1 WHERE id = $2", HashSecret(consumerLike), planted.ID)
	_, err = s.ByToken(t.Context(), consumerLike)
	wantUnauthenticated(t, "an mhc_ secret stored as a token hash", err)
	_, err = s.ByToken(t.Context(), "paulo santos")
	wantUnauthenticated(t, "plain text", err)

	yes := true
	if err := s.UpdateUser(t.Context(), "paulo-santos", UserChange{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	_, err = s.ByToken(t.Context(), secret)
	wantUnauthenticated(t, "a token of a disabled person", err)
}

func TestLastUseIsRecordedAtMostOncePerMinute(t *testing.T) {
	s, pool := newService(t)
	prepare(t, s)
	create(t, s, "paulo-santos", false)
	setPermissions(t, s, "paulo-santos", "monitor:sei:hook")
	paulo := principalOf(t, s, "paulo-santos")
	hook := TokenRequest{Label: "alerts", Permissions: permissions(t, "monitor:sei:hook")}
	_, token := createToken(t, s, paulo, hook)
	_, credential, err := s.CreateCredential(t.Context(), paulo, "alerts-paulo", hook)
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]struct {
		use      func(svc *Service) error
		lastUsed func() *time.Time
	}{
		"token": {
			use: func(svc *Service) error { _, err := svc.ByToken(t.Context(), token); return err },
			lastUsed: func() *time.Time {
				tokens, err := s.Tokens(t.Context(), paulo.UserID())
				if err != nil || len(tokens) != 1 {
					t.Fatalf("tokens of paulo-santos: %d, %v", len(tokens), err)
				}
				return tokens[0].LastUsedAt
			},
		},
		"credential": {
			use: func(svc *Service) error {
				_, err := svc.ByCredential(t.Context(), "alerts-paulo", credential)
				return err
			},
			lastUsed: func() *time.Time {
				credentials, err := s.Credentials(t.Context(), paulo.UserID())
				if err != nil || len(credentials) != 1 {
					t.Fatalf("credentials of paulo-santos: %d, %v", len(credentials), err)
				}
				return credentials[0].LastUsedAt
			},
		},
	}
	for name, kind := range kinds {
		if got := kind.lastUsed(); got != nil {
			t.Errorf("%s: last use before any use = %v; want none", name, got)
		}
		uses := []struct {
			at, want time.Duration
		}{{0, 0}, {30 * time.Second, 0}, {time.Minute, 0}, {61 * time.Second, 61 * time.Second}}
		for _, u := range uses {
			if err := kind.use(serviceAt(pool, u.at)); err != nil {
				t.Fatalf("%s: use at %v: %v", name, u.at, err)
			}
			if got := kind.lastUsed(); got == nil || !got.Equal(testNow.Add(u.want)) {
				t.Errorf("%s: last use after a use at %v = %v; want %v", name, u.at, got, testNow.Add(u.want))
			}
		}
	}
}

func TestRevokeFindsOnlyTheOwnersTokens(t *testing.T) {
	s, pool := newService(t)
	prepare(t, s)
	create(t, s, "paulo-santos", false)
	create(t, s, "ana-lima", false)
	setPermissions(t, s, "paulo-santos", "monitor:sei:hook")
	setPermissions(t, s, "ana-lima", "monitor:sei:hook")
	paulo := principalOf(t, s, "paulo-santos")
	ana := principalOf(t, s, "ana-lima")
	hook := permissions(t, "monitor:sei:hook")

	kinds := map[string]struct {
		create  func(svc *Service, owner access.Principal, label string) int64
		list    func(userID int64) []string
		revoke  func(id, userID int64) error
		unknown error
		text    string
	}{
		"token": {
			create: func(svc *Service, owner access.Principal, label string) int64 {
				token, _ := createToken(t, svc, owner, TokenRequest{Label: label, Permissions: hook})
				return token.ID
			},
			list: func(userID int64) []string {
				tokens, err := s.Tokens(t.Context(), userID)
				if err != nil {
					t.Fatal(err)
				}
				out := []string{}
				for _, k := range tokens {
					out = append(out, fmt.Sprintf("%s/%s %v", k.Owner, k.Label, k.Permissions))
				}
				return out
			},
			revoke:  func(id, userID int64) error { return s.RevokeToken(t.Context(), id, userID) },
			unknown: ErrUnknownToken,
			text:    "no token has this id",
		},
		"credential": {
			create: func(svc *Service, owner access.Principal, label string) int64 {
				credential, _, err := svc.CreateCredential(t.Context(), owner, "c-"+label, TokenRequest{Label: label, Permissions: hook})
				if err != nil {
					t.Fatalf("create the credential %s: %v", label, err)
				}
				return credential.ID
			},
			list: func(userID int64) []string {
				credentials, err := s.Credentials(t.Context(), userID)
				if err != nil {
					t.Fatal(err)
				}
				out := []string{}
				for _, c := range credentials {
					out = append(out, fmt.Sprintf("%s/%s %v", c.Owner, c.Username, c.Permissions))
				}
				return out
			},
			revoke:  func(id, userID int64) error { return s.RevokeCredential(t.Context(), id, userID) },
			unknown: ErrUnknownCredential,
			text:    "no credential has this id",
		},
	}
	for name, kind := range kinds {
		kind.create(serviceAt(pool, time.Minute), paulo, "later")
		earlier := kind.create(s, paulo, "earlier")
		anas := kind.create(s, ana, "ana")
		list := func(what string, userID int64, want ...string) {
			t.Helper()
			if got := kind.list(userID); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: %s = %v; want %v", name, what, got, want)
			}
		}
		prefix := map[string]string{"token": "", "credential": "c-"}[name]
		item := func(owner, label string) string {
			return owner + "/" + prefix + label + " [monitor:sei:hook]"
		}

		list("everyone's", 0, item("paulo-santos", "earlier"), item("ana-lima", "ana"), item("paulo-santos", "later"))
		list("paulo-santos's", paulo.UserID(), item("paulo-santos", "earlier"), item("paulo-santos", "later"))

		if err := kind.revoke(earlier, ana.UserID()); !errors.Is(err, kind.unknown) || err.Error() != kind.text {
			t.Errorf("%s: ana-lima revoking one of paulo-santos: err = %v; want %q", name, err, kind.text)
		}
		if err := kind.revoke(earlier, paulo.UserID()); err != nil {
			t.Errorf("%s: paulo-santos revoking his own: %v", name, err)
		}
		if err := kind.revoke(anas, 0); err != nil {
			t.Errorf("%s: revoking with no owner: %v", name, err)
		}
		if err := kind.revoke(anas, 0); !errors.Is(err, kind.unknown) {
			t.Errorf("%s: revoking twice: err = %v; want %v", name, err, kind.unknown)
		}
		list("everyone's after the revocations", 0, item("paulo-santos", "later"))
	}
}
