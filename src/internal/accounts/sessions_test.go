package accounts

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/access"
)

func login(t *testing.T, s *Service, name string) Session {
	t.Helper()
	session, _, err := s.Login(t.Context(), name, password)
	if err != nil {
		t.Fatalf("login %s: %v", name, err)
	}
	return session
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoginOpensASessionForTwelveHours(t *testing.T) {
	s, pool := newService(t)
	create(t, s, "paulo-santos", true)
	create(t, s, "ana-lima", false)
	exec(t, pool, `INSERT INTO sessions (id_hash, user_id, expires_at)
		SELECT 'expired', id, $1::timestamptz FROM users WHERE login = 'ana-lima'
		UNION ALL SELECT 'open', id, $2::timestamptz FROM users WHERE login = 'ana-lima'`,
		testNow, testNow.Add(time.Second))

	session, principal, err := s.Login(t.Context(), "paulo-santos", password)
	if err != nil {
		t.Fatal(err)
	}

	if want := testNow.Add(12 * time.Hour); !session.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v; want %v", session.ExpiresAt, want)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(session.ID) {
		t.Error("the session id is not 43 base64url characters")
	}
	if principal.Login() != "paulo-santos" || !principal.Admin() {
		t.Errorf("Login principal = %q admin %v; want paulo-santos admin", principal.Login(), principal.Admin())
	}
	found, err := s.BySession(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found.Kind() != access.Session || found.Login() != "paulo-santos" || !found.Admin() {
		t.Errorf("BySession = kind %v %q admin %v; want a session of paulo-santos with admin", found.Kind(), found.Login(), found.Admin())
	}
	if countRows(t, pool, "SELECT count(*) FROM sessions WHERE id_hash = $1", session.ID) != 0 {
		t.Error("sessions stores the session id in clear")
	}
	stored := countRows(t, pool, "SELECT count(*) FROM sessions WHERE id_hash = $1 AND expires_at = $2",
		HashSecret(session.ID), testNow.Add(SessionLifetime))
	if stored != 1 {
		t.Error("sessions does not store the hash of the session id with its expiry")
	}
	var left []string
	rows, err := pool.Query(t.Context(), "SELECT id_hash FROM sessions WHERE id_hash IN ('expired', 'open')")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != "open" {
		t.Errorf("sessions of other people after a login = %v; want only the open one", left)
	}
}

func TestWrongLoginsLookTheSame(t *testing.T) {
	s, pool := newService(t)
	create(t, s, "paulo-santos", false)
	create(t, s, "ana-lima", false)
	yes := true
	if err := s.UpdateUser(t.Context(), "ana-lima", UserChange{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}

	attempts := map[string][2]string{
		"an unknown login":  {"ghost", password},
		"a wrong password":  {"paulo-santos", "outra senha bem comprida"},
		"a disabled person": {"ana-lima", password},
	}
	for name, attempt := range attempts {
		_, _, err := s.Login(t.Context(), attempt[0], attempt[1])
		if !errors.Is(err, ErrWrongPassword) || err.Error() != "wrong login or password" {
			t.Errorf("%s: err = %v; want ErrWrongPassword", name, err)
		}
	}
	if n := countRows(t, pool, "SELECT count(*) FROM sessions"); n != 0 {
		t.Errorf("sessions after wrong logins = %d; want 0", n)
	}
	if ok, err := VerifyPassword(unknownLoginHash, password); ok || err != nil {
		t.Errorf("the hash for unknown logins: VerifyPassword = %v, %v; want false, nil", ok, err)
	}
}

func TestASessionEnds(t *testing.T) {
	s, pool := newService(t)
	create(t, s, "paulo-santos", false)
	create(t, s, "ana-lima", false)
	expiring := login(t, s, "paulo-santos")
	loggedOut := login(t, s, "paulo-santos")
	kept := login(t, s, "paulo-santos")
	disabled := login(t, s, "ana-lima")
	at := func(d time.Duration) *Service {
		return NewService(pool, func() time.Time { return testNow.Add(d) })
	}
	ended := func(name string, svc *Service, session Session) {
		t.Helper()
		if _, err := svc.BySession(t.Context(), session.ID); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: err = %v; want ErrUnauthenticated", name, err)
		}
	}
	open := func(name string, session Session) {
		t.Helper()
		if _, err := s.BySession(t.Context(), session.ID); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	if _, err := at(SessionLifetime-time.Second).BySession(t.Context(), expiring.ID); err != nil {
		t.Errorf("a second before 12 hours: %v", err)
	}
	ended("after 12 hours", at(SessionLifetime), expiring)
	ended("an unknown session", s, Session{ID: "unknown"})

	if err := s.Logout(t.Context(), loggedOut.ID); err != nil {
		t.Fatal(err)
	}
	ended("after logout", s, loggedOut)
	open("another session of the same person after logout", kept)

	yes := true
	if err := s.UpdateUser(t.Context(), "ana-lima", UserChange{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	ended("a person disabled after login", s, disabled)
	open("a session of someone else after a disable", kept)
	no := false
	if err := s.UpdateUser(t.Context(), "ana-lima", UserChange{Disabled: &no}); err != nil {
		t.Fatal(err)
	}
	ended("a person disabled after login and enabled again", s, disabled)

	if err := s.DeleteUser(t.Context(), "paulo-santos"); err != nil {
		t.Fatal(err)
	}
	ended("a deleted person", s, kept)
}
