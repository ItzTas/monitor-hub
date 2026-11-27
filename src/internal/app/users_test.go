package app

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

const (
	examplePassword = "uma senha longa demais"
	changedPassword = "outra senha bem comprida"
	weakPassword    = "curta"
)

// runUsers fails the test if a password shows in the output, and returns the
// output with the passwords redacted, so failure messages never print one.
func runUsers(t *testing.T, environ []string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(t.Context(), append([]string{"users"}, args...), environ, strings.NewReader(stdin), &stdout, &stderr)
	return code, redacted(t, "stdout", stdout.String()), redacted(t, "stderr", stderr.String())
}

func redacted(t *testing.T, stream, output string) string {
	t.Helper()
	passwords := map[string]string{"the example password": examplePassword, "the changed password": changedPassword, "the weak password": weakPassword}
	for name, password := range passwords {
		if strings.Contains(output, password) {
			t.Errorf("%s shows %s", stream, name)
			output = strings.ReplaceAll(output, password, "[redacted]")
		}
	}
	return output
}

func usersDatabase(t *testing.T) ([]string, *accounts.Service, *pgxpool.Pool) {
	t.Helper()
	address := coretest.Database(t)
	pool, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return []string{"DATABASE_URL=" + address}, accounts.NewService(pool, time.Now), pool
}

func passwordOf(t *testing.T, pool *pgxpool.Pool, login, password string) bool {
	t.Helper()
	var hash string
	if err := pool.QueryRow(t.Context(), "SELECT password_hash FROM users WHERE login = $1", login).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	ok, err := accounts.VerifyPassword(hash, password)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestUsersCreateAdminReadsThePasswordFromStdin(t *testing.T) {
	environ, _, pool := usersDatabase(t)

	code, out, errOut := runUsers(t, environ, examplePassword+"\n", "create", "--admin", "paulo-santos")

	if code != 0 || out != "person paulo-santos created as an admin\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if !passwordOf(t, pool, "paulo-santos", examplePassword) {
		t.Error("the stored password is not the first line of stdin")
	}
}

func TestUsersListShowsTheAdminMarkAndState(t *testing.T) {
	environ, svc, _ := usersDatabase(t)
	code, out, errOut := runUsers(t, environ, "", "list")
	if code != 0 || out != "no people\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	runUsers(t, environ, examplePassword, "create", "--admin", "paulo-santos")
	runUsers(t, environ, examplePassword, "create", "ana-lima")
	disabled := true
	if err := svc.UpdateUser(t.Context(), "ana-lima", accounts.UserChange{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}

	code, out, errOut = runUsers(t, environ, "", "list")

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if code != 0 || len(lines) != 3 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	want := [][]string{
		{"LOGIN", "ADMIN", "DISABLED", "CREATED"},
		{"ana-lima", "no", "yes"},
		{"paulo-santos", "yes", "no"},
	}
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 4 || strings.Join(fields[:len(want[i])], " ") != strings.Join(want[i], " ") {
			t.Errorf("line %d = %q; want it to start with %q", i, line, want[i])
		}
		if i > 0 {
			if _, err := time.Parse(time.RFC3339, fields[3]); err != nil {
				t.Errorf("line %d: created %q: %v", i, fields[3], err)
			}
		}
	}
}

func TestUsersPasswdAndDelete(t *testing.T) {
	environ, _, pool := usersDatabase(t)
	code, out, errOut := runUsers(t, environ, examplePassword+"\n", "create", "ana-lima")
	if code != 0 || out != "person ana-lima created\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = runUsers(t, environ, changedPassword+"\n", "passwd", "ana-lima")

	if code != 0 || out != "password of ana-lima changed; their sessions ended\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if !passwordOf(t, pool, "ana-lima", changedPassword) {
		t.Error("the changed password is not stored")
	}
	code, out, errOut = runUsers(t, environ, "", "delete", "ana-lima")
	if code != 0 || out != "person ana-lima deleted\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	for _, args := range [][]string{{"delete", "ana-lima"}, {"passwd", "ana-lima"}} {
		code, out, errOut = runUsers(t, environ, changedPassword+"\n", args...)
		if code != 1 || !strings.Contains(out+errOut, "no person has this login") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

func TestUsersCreateRefusesAWeakPassword(t *testing.T) {
	environ, _, _ := usersDatabase(t)

	code, out, errOut := runUsers(t, environ, weakPassword+"\n", "create", "paulo-santos")

	if code != 1 || !strings.Contains(out+errOut, "use a password of at least 12 characters and at most 256 bytes") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestUsersUsageErrors(t *testing.T) {
	usage := "usage: monitor-hub users create [--admin] <login> | passwd <login> | list | delete <login>\n"
	invalid := accounts.ErrInvalidLogin.Error() + "\n"
	cases := map[string]struct {
		args []string
		want string
	}{
		"no subcommand":         {nil, usage},
		"create without login":  {[]string{"create"}, usage},
		"admin without login":   {[]string{"create", "--admin"}, usage},
		"unknown subcommand":    {[]string{"remove", "x"}, usage},
		"list with an argument": {[]string{"list", "x"}, usage},
		"admin on passwd":       {[]string{"passwd", "--admin", "paulo-santos"}, usage},
		"invalid login":         {[]string{"create", "Paulo"}, invalid},
		"invalid admin login":   {[]string{"create", "--admin", "paulo--santos"}, invalid},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runUsers(t, nil, examplePassword, tt.args...)
			if code != 2 || errOut != tt.want || out != "" {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
		})
	}
}

func TestUsersDeleteRefusesTheLastAdmin(t *testing.T) {
	environ, _, _ := usersDatabase(t)
	runUsers(t, environ, examplePassword, "create", "--admin", "paulo-santos")

	code, out, errOut := runUsers(t, environ, "", "delete", "paulo-santos")

	if code != 1 || !strings.Contains(out+errOut, "this is the last person with admin") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}
