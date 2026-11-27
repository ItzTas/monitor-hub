package accounts

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ItzTas/monitor-hub/src/internal/access"
)

func TestACredentialAuthenticatesByUsernameAndPassword(t *testing.T) {
	s, pool := newService(t)
	prepare(t, s)
	create(t, s, "paulo-santos", false)
	setPermissions(t, s, "paulo-santos", "monitor:sei:view", "monitor:sei:hook")
	paulo := principalOf(t, s, "paulo-santos")
	hook := TokenRequest{Label: "alerts", Permissions: permissions(t, "monitor:sei:hook")}

	credential, secret, err := s.CreateCredential(t.Context(), paulo, "alerts-paulo", hook)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^mhk_[A-Za-z0-9_-]{43}$`).MatchString(secret) {
		t.Error("the password is not mhk_ followed by 43 base64url characters")
	}
	want := Credential{
		Token:    Token{ID: credential.ID, Owner: "paulo-santos", Label: "alerts", Permissions: hook.Permissions, CreatedAt: testNow},
		Username: "alerts-paulo",
	}
	if !reflect.DeepEqual(credential, want) {
		t.Errorf("CreateCredential = %+v; want %+v", credential, want)
	}
	expiry := testNow.Add(time.Hour)
	_, expiring, err := s.CreateCredential(t.Context(), paulo, "expiring-paulo",
		TokenRequest{Label: "alerts", Permissions: hook.Permissions, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}

	p, err := s.ByCredential(t.Context(), "alerts-paulo", secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind() != access.Credential || p.Login() != "paulo-santos" || !p.Can(access.Hook, "sei") || p.Can(access.View, "sei") {
		t.Errorf("ByCredential = kind %v %q hook sei %v view sei %v; want a credential of paulo-santos with only hook",
			p.Kind(), p.Login(), p.Can(access.Hook, "sei"), p.Can(access.View, "sei"))
	}

	_, err = s.ByCredential(t.Context(), "alerts-paulo", "mhk_"+strings.Repeat("a", 43))
	wantUnauthenticated(t, "a wrong password", err)
	_, err = s.ByCredential(t.Context(), "ghost", secret)
	wantUnauthenticated(t, "an unknown username", err)
	if _, err := serviceAt(pool, time.Hour-time.Second).ByCredential(t.Context(), "expiring-paulo", expiring); err != nil {
		t.Errorf("a second before the expiry: %v", err)
	}
	_, err = serviceAt(pool, time.Hour).ByCredential(t.Context(), "expiring-paulo", expiring)
	wantUnauthenticated(t, "at the expiry", err)

	if _, _, err := s.CreateCredential(t.Context(), paulo, "Paulo", hook); !errors.Is(err, ErrInvalidUsername) ||
		err.Error() != "usernames use lowercase letters, digits and single hyphens, 2 to 31 characters, starting with a letter" {
		t.Errorf("username Paulo: err = %v; want ErrInvalidUsername", err)
	}
	if _, _, err := s.CreateCredential(t.Context(), paulo, "alerts-paulo", hook); !errors.Is(err, ErrUsernameTaken) ||
		err.Error() != "a credential already uses this username" {
		t.Errorf("the same username twice: err = %v; want ErrUsernameTaken", err)
	}
	if _, _, err := s.CreateCredential(t.Context(), paulo, "paulo-santos", hook); err != nil {
		t.Errorf("a username equal to a login: %v", err)
	}

	setPermissions(t, s, "paulo-santos", "monitor:sei:view")
	p, err = s.ByCredential(t.Context(), "alerts-paulo", secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Can(access.Hook, "sei") {
		t.Error("the credential still hooks sei after its owner lost hook")
	}

	yes := true
	if err := s.UpdateUser(t.Context(), "paulo-santos", UserChange{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	_, err = s.ByCredential(t.Context(), "alerts-paulo", secret)
	wantUnauthenticated(t, "a credential of a disabled person", err)
}

func TestSecretsAreStoredOnlyAsHashes(t *testing.T) {
	s, pool := newService(t)
	prepare(t, s)
	create(t, s, "paulo-santos", false)
	setPermissions(t, s, "paulo-santos", "monitor:sei:view", "monitor:sei:hook")
	paulo := principalOf(t, s, "paulo-santos")
	_, token := createToken(t, s, paulo, TokenRequest{Label: "rss", Permissions: permissions(t, "monitor:sei:view")})
	_, secret, err := s.CreateCredential(t.Context(), paulo, "alerts-paulo",
		TokenRequest{Label: "alerts", Permissions: permissions(t, "monitor:sei:hook")})
	if err != nil {
		t.Fatal(err)
	}

	var tokenHash, secretHash string
	if err := pool.QueryRow(t.Context(), "SELECT hash FROM tokens").Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT secret_hash FROM credentials").Scan(&secretHash); err != nil {
		t.Fatal(err)
	}
	if tokenHash != HashSecret(token) {
		t.Error("tokens.hash is not HashSecret of the token")
	}
	if secretHash != HashSecret(secret) {
		t.Error("credentials.secret_hash is not HashSecret of the password")
	}

	secrets := map[string]string{
		"the token":                       token,
		"the token without its prefix":    token[4:],
		"the password":                    secret,
		"the password without its prefix": secret[4:],
	}
	tables, err := pool.Query(t.Context(),
		"SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'")
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(tables, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	scanned := map[string]int{}
	for _, table := range names {
		rows, err := pool.Query(t.Context(), "SELECT t::text FROM "+pgx.Identifier{table}.Sanitize()+" t")
		if err != nil {
			t.Fatal(err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		scanned[table] = len(texts)
		for _, text := range texts {
			for name, value := range secrets {
				if strings.Contains(text, value) {
					t.Errorf("a row of %s holds %s", table, name)
				}
			}
		}
	}
	if scanned["tokens"] != 1 || scanned["credentials"] != 1 {
		t.Errorf("rows scanned = %v; want one in tokens and one in credentials", scanned)
	}
}
