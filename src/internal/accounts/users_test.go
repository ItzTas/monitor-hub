package accounts

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/access"
)

const password = "uma senha longa demais"

func create(t *testing.T, s *Service, login string, admin bool) {
	t.Helper()
	if err := s.CreateUser(t.Context(), login, password, admin); err != nil {
		t.Fatalf("create %s: %v", login, err)
	}
}

type state struct {
	Login    string
	Admin    bool
	Disabled bool
}

func states(t *testing.T, s *Service) []state {
	t.Helper()
	users, err := s.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := []state{}
	for _, u := range users {
		out = append(out, state{u.Login, u.Admin, u.Disabled})
	}
	return out
}

func storedHash(t *testing.T, pool *pgxpool.Pool, login string) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(t.Context(), "SELECT password_hash FROM users WHERE login = $1", login).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestCreateUserValidates(t *testing.T) {
	s, pool := newService(t)

	for _, login := range []string{"Paulo", "p", "paulo-", "paulo--santos"} {
		if err := s.CreateUser(t.Context(), login, password, false); !errors.Is(err, ErrInvalidLogin) {
			t.Errorf("login %q: err = %v; want ErrInvalidLogin", login, err)
		}
	}
	weak := map[string]string{
		"11 characters": "abcdefghijk",
		"257 bytes":     strings.Repeat("a", 257),
	}
	for name, weakPassword := range weak {
		if err := s.CreateUser(t.Context(), "paulo-santos", weakPassword, false); !errors.Is(err, ErrWeakPassword) {
			t.Errorf("a password of %s: err = %v; want ErrWeakPassword", name, err)
		}
	}
	if got := states(t, s); len(got) != 0 {
		t.Fatalf("people after refused creations: %v", got)
	}

	create(t, s, "paulo-santos", false)
	if err := s.CreateUser(t.Context(), "paulo-santos", password, true); !errors.Is(err, ErrUserExists) {
		t.Errorf("the same login twice: err = %v; want ErrUserExists", err)
	}

	expect := []state{{Login: "paulo-santos"}}
	if got := states(t, s); !reflect.DeepEqual(got, expect) {
		t.Errorf("people = %v; want %v", got, expect)
	}
	hash := storedHash(t, pool, "paulo-santos")
	if ok, err := VerifyPassword(hash, password); !ok || err != nil || !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("the stored password does not verify as argon2id: %v, %v", ok, err)
	}
}

func TestPrepareCreatesTheMonitorRows(t *testing.T) {
	s, pool := newService(t)
	exec(t, pool, `INSERT INTO monitors (name, display_name, version) VALUES ('sei', 'SEI', '2.4.0')`)

	for range 2 {
		if err := s.Prepare(t.Context(), []string{"diario", "sei"}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := pool.Query(t.Context(), "SELECT name || '|' || display_name || '|' || version FROM monitors ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"diario||", "sei|SEI|2.4.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("monitors = %v; want %v", got, want)
	}
	names, err := s.MonitorNames(t.Context())
	if want := []string{"diario", "sei"}; err != nil || !reflect.DeepEqual(names, want) {
		t.Errorf("MonitorNames = %v, %v; want %v", names, err, want)
	}
}

func TestTheLastAdminCannotBeDeleted(t *testing.T) {
	s, _ := newService(t)
	create(t, s, "paulo-santos", true)
	create(t, s, "ana-lima", false)
	yes, no := true, false
	before := states(t, s)

	refused := map[string]func() error{
		"delete":       func() error { return s.DeleteUser(t.Context(), "paulo-santos") },
		"disable":      func() error { return s.UpdateUser(t.Context(), "paulo-santos", UserChange{Disabled: &yes}) },
		"remove admin": func() error { return s.UpdateUser(t.Context(), "paulo-santos", UserChange{Admin: &no}) },
	}
	for name, change := range refused {
		if err := change(); !errors.Is(err, ErrLastAdmin) {
			t.Errorf("%s the last admin: err = %v; want ErrLastAdmin", name, err)
		}
	}
	if got := states(t, s); !reflect.DeepEqual(got, before) {
		t.Fatalf("people after refused changes = %v; want %v", got, before)
	}

	if err := s.DeleteUser(t.Context(), "ana-lima"); err != nil {
		t.Fatalf("delete ana-lima without admin: %v", err)
	}
	create(t, s, "ana-lima", false)
	if err := s.UpdateUser(t.Context(), "ana-lima", UserChange{Admin: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(t.Context(), "paulo-santos", UserChange{Admin: &no}); err != nil {
		t.Fatalf("remove admin from paulo-santos once ana-lima has it: %v", err)
	}
	if err := s.UpdateUser(t.Context(), "ana-lima", UserChange{Admin: &yes, Disabled: &yes}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("keep admin but disable the last admin: err = %v; want ErrLastAdmin", err)
	}
	want := []state{{Login: "ana-lima", Admin: true}, {Login: "paulo-santos"}}
	if got := states(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("people = %v; want %v", got, want)
	}
	if err := s.DeleteUser(t.Context(), "nobody"); !errors.Is(err, ErrUnknownUser) {
		t.Errorf("delete a missing person: err = %v; want ErrUnknownUser", err)
	}
	if err := s.UpdateUser(t.Context(), "nobody", UserChange{Admin: &yes}); !errors.Is(err, ErrUnknownUser) {
		t.Errorf("update a missing person: err = %v; want ErrUnknownUser", err)
	}

	withoutAdmins, _ := newService(t)
	create(t, withoutAdmins, "ana-lima", false)
	create(t, withoutAdmins, "paulo-santos", false)
	if err := withoutAdmins.UpdateUser(t.Context(), "ana-lima", UserChange{Disabled: &yes}); err != nil {
		t.Fatalf("disable ana-lima with no admin anywhere: %v", err)
	}
	if err := withoutAdmins.DeleteUser(t.Context(), "paulo-santos"); err != nil {
		t.Fatalf("delete paulo-santos with no admin anywhere: %v", err)
	}
	if want, got := []state{{Login: "ana-lima", Disabled: true}}, states(t, withoutAdmins); !reflect.DeepEqual(got, want) {
		t.Errorf("people with no admin = %v; want %v", got, want)
	}
}

func TestSetPasswordEndsTheSessions(t *testing.T) {
	s, pool := newService(t)
	create(t, s, "paulo-santos", true)
	create(t, s, "ana-lima", false)
	exec(t, pool, `INSERT INTO sessions (id_hash, user_id, expires_at)
		SELECT login || '-session', id, now() + interval '12 hours' FROM users`)
	const changed = "outra senha bem comprida"

	if err := s.SetPassword(t.Context(), "paulo-santos", changed); err != nil {
		t.Fatal(err)
	}

	var sessions []string
	rows, err := pool.Query(t.Context(), "SELECT id_hash FROM sessions ORDER BY id_hash")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ana-lima-session"}; !reflect.DeepEqual(sessions, want) {
		t.Errorf("sessions = %v; want %v", sessions, want)
	}
	if ok, err := VerifyPassword(storedHash(t, pool, "paulo-santos"), changed); !ok || err != nil {
		t.Errorf("the new password does not verify: %v, %v", ok, err)
	}
	if err := s.SetPassword(t.Context(), "paulo-santos", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("a short password: err = %v; want ErrWeakPassword", err)
	}
	if err := s.SetPassword(t.Context(), "nobody", changed); !errors.Is(err, ErrUnknownUser) {
		t.Errorf("a missing person: err = %v; want ErrUnknownUser", err)
	}
}

func TestListUsersShowsThePermissions(t *testing.T) {
	s, pool := newService(t)
	if err := s.Prepare(t.Context(), []string{"sei"}); err != nil {
		t.Fatal(err)
	}
	create(t, s, "paulo-santos", false)
	create(t, s, "ana-lima", false)
	exec(t, pool, `INSERT INTO user_grants (user_id, monitor, action)
		SELECT id, 'sei', 'view' FROM users WHERE login = 'paulo-santos'
		UNION ALL SELECT id, NULL, 'check' FROM users WHERE login = 'paulo-santos'`)

	users, err := s.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	got := map[string][]access.Permission{}
	for _, u := range users {
		got[u.Login] = u.Permissions
		if !u.CreatedAt.Equal(testNow) {
			t.Errorf("%s created at %v; want %v", u.Login, u.CreatedAt, testNow)
		}
	}
	want := map[string][]access.Permission{
		"ana-lima":     nil,
		"paulo-santos": {{Monitor: access.Every, Action: access.Check}, {Monitor: "sei", Action: access.View}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("permissions = %v; want %v", got, want)
	}
}
