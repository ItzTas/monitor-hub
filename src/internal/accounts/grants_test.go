package accounts

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/access"
)

func prepare(t *testing.T, s *Service) {
	t.Helper()
	if err := s.Prepare(t.Context(), []string{"diario", "sei"}); err != nil {
		t.Fatal(err)
	}
}

func permissions(t *testing.T, slugs ...string) []access.Permission {
	t.Helper()
	perms := make([]access.Permission, 0, len(slugs))
	for _, slug := range slugs {
		p, err := access.Parse(slug)
		if err != nil {
			t.Fatal(err)
		}
		perms = append(perms, p)
	}
	return perms
}

func setPermissions(t *testing.T, s *Service, login string, slugs ...string) {
	t.Helper()
	if err := s.SetPermissions(t.Context(), login, permissions(t, slugs...)); err != nil {
		t.Fatalf("set the permissions of %s: %v", login, err)
	}
}

func granted(t *testing.T, s *Service) map[string][]string {
	t.Helper()
	users, err := s.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, u := range users {
		for _, p := range u.Permissions {
			out[u.Login] = append(out[u.Login], p.String())
		}
	}
	return out
}

func TestSetPermissionsReplacesTheList(t *testing.T) {
	s, _ := newService(t)
	prepare(t, s)
	create(t, s, "paulo-santos", false)
	create(t, s, "ana-lima", false)
	setPermissions(t, s, "paulo-santos", "monitor:sei:view")
	setPermissions(t, s, "ana-lima", "monitor:sei:view")
	session := login(t, s, "ana-lima")
	before, err := s.BySession(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Can(access.View, "sei") {
		t.Fatal("ana-lima cannot view sei after the first list")
	}

	setPermissions(t, s, "ana-lima", "monitor:*:check", "monitor:*:check")

	after, err := s.BySession(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Can(access.View, "sei") || !after.Can(access.Check, "diario") {
		t.Errorf("the open session of ana-lima: view sei %v, check diario %v; want false, true",
			after.Can(access.View, "sei"), after.Can(access.Check, "diario"))
	}
	want := map[string][]string{"ana-lima": {"monitor:*:check"}, "paulo-santos": {"monitor:sei:view"}}
	if got := granted(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("permissions = %v; want %v", got, want)
	}

	err = s.SetPermissions(t.Context(), "ana-lima", permissions(t, "monitor:sei:view", "monitor:nope:view"))
	if !errors.Is(err, ErrUnknownMonitor) || err.Error() != "no monitor has this name" {
		t.Errorf("a missing monitor: err = %v; want ErrUnknownMonitor", err)
	}
	if got := granted(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("permissions after a refused list = %v; want %v", got, want)
	}
	if err := s.SetPermissions(t.Context(), "ghost", permissions(t, "monitor:sei:view")); !errors.Is(err, ErrUnknownUser) {
		t.Errorf("a missing person: err = %v; want ErrUnknownUser", err)
	}
}

func TestMeListsGrantsAndWhatThePersonCanDo(t *testing.T) {
	s, _ := newService(t)
	prepare(t, s)
	create(t, s, "paulo-santos", false)
	create(t, s, "ana-lima", true)
	setPermissions(t, s, "paulo-santos", "monitor:sei:view", "monitor:*:hook")
	me := func(login string) Me {
		t.Helper()
		_, principal, err := s.Login(t.Context(), login, password)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Me(t.Context(), principal)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	want := Me{
		Login:       "paulo-santos",
		Permissions: []string{"monitor:*:hook", "monitor:sei:view"},
		Monitors:    map[string][]access.Action{"diario": {access.Hook}, "sei": {access.View, access.Hook}},
	}
	if got := me("paulo-santos"); !reflect.DeepEqual(got, want) {
		t.Errorf("Me = %+v; want %+v", got, want)
	}

	admin, err := json.Marshal(me("ana-lima"))
	if err != nil {
		t.Fatal(err)
	}
	const wantAdmin = `{"login":"ana-lima","admin":true,"permissions":[],` +
		`"monitors":{"diario":["view","check","hook","configure"],"sei":["view","check","hook","configure"]}}`
	if string(admin) != wantAdmin {
		t.Errorf("Me of an admin = %s; want %s", admin, wantAdmin)
	}

	// Postgres puts sei before sei-x; the slugs sort the other way.
	if err := s.Prepare(t.Context(), []string{"sei-x"}); err != nil {
		t.Fatal(err)
	}
	setPermissions(t, s, "ana-lima", "monitor:sei:check", "monitor:sei-x:view")
	got := me("ana-lima")
	if want := []string{"monitor:sei-x:view", "monitor:sei:check"}; !reflect.DeepEqual(got.Permissions, want) {
		t.Errorf("permissions of an admin = %v; want %v", got.Permissions, want)
	}
	if len(got.Monitors) != 3 || len(got.Monitors["sei-x"]) != 4 {
		t.Errorf("monitors of an admin = %v; want the four actions on diario, sei and sei-x", got.Monitors)
	}

	setPermissions(t, s, "paulo-santos", "monitor:sei:view")
	if got, want := me("paulo-santos").Monitors, map[string][]access.Action{"sei": {access.View}}; !reflect.DeepEqual(got, want) {
		t.Errorf("monitors with one grant = %v; want %v", got, want)
	}
}
