// Package accountstest creates people, sessions and tokens for tests.
package accountstest

import (
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
)

const Password = "paulo santos test password"

// Person creates login with Password and the given permission slugs, first giving
// the monitors they name a row.
func Person(t testing.TB, svc *accounts.Service, login string, permissions ...string) {
	t.Helper()
	perms := parse(t, permissions)
	monitors := []string{}
	for _, p := range perms {
		if p.Monitor != access.Every {
			monitors = append(monitors, p.Monitor)
		}
	}
	if err := svc.Prepare(t.Context(), monitors); err != nil {
		t.Fatal(err)
	}
	if err := svc.CreateUser(t.Context(), login, Password, false); err != nil {
		t.Fatalf("create %s: %v", login, err)
	}
	if err := svc.SetPermissions(t.Context(), login, perms); err != nil {
		t.Fatalf("set the permissions of %s: %v", login, err)
	}
}

func Admin(t testing.TB, svc *accounts.Service, login string) {
	t.Helper()
	if err := svc.CreateUser(t.Context(), login, Password, true); err != nil {
		t.Fatalf("create %s: %v", login, err)
	}
}

// Session returns the id of a new session of login.
func Session(t testing.TB, svc *accounts.Service, login string) string {
	t.Helper()
	session, _, err := svc.Login(t.Context(), login, Password)
	if err != nil {
		t.Fatalf("login %s: %v", login, err)
	}
	return session.ID
}

// Token returns a mht_ token that a session of login creates with the permission slugs.
func Token(t testing.TB, svc *accounts.Service, login string, permissions ...string) string {
	t.Helper()
	_, owner, err := svc.Login(t.Context(), login, Password)
	if err != nil {
		t.Fatalf("login %s: %v", login, err)
	}
	_, secret, err := svc.CreateToken(t.Context(), owner, accounts.TokenRequest{Label: "test", Permissions: parse(t, permissions)})
	if err != nil {
		t.Fatalf("create a token of %s: %v", login, err)
	}
	return secret
}

func parse(t testing.TB, slugs []string) []access.Permission {
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
