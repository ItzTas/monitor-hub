package app

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

func TestUnknownCommandListsTheKnownOnes(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{"serv"}, nil, strings.NewReader(""), &stdout, &stderr)

	if code != 2 || !strings.Contains(stderr.String(), `unknown command "serv"`) || !strings.Contains(stderr.String(), "serve") || stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestServeTakesNoArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{"serve", "now"}, nil, strings.NewReader(""), &stdout, &stderr)

	if code != 2 || stderr.String() != "serve takes no arguments\n" || stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestServeStopsOnAnInvalidConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), nil, []string{"API_TOKEN=api-secret-value", "FEED_TOKEN=feed-secret-value"}, strings.NewReader(""), &stdout, &stderr)

	output := stdout.String() + stderr.String()
	if code != 1 || !strings.Contains(output, "DATABASE_URL is empty") {
		t.Errorf("exit %d, output %q", code, output)
	}
	for _, secret := range []string{"api-secret-value", "feed-secret-value"} {
		if strings.Contains(output, secret) {
			t.Errorf("the output shows %q: %s", secret, output)
		}
	}
}

func runConsumers(t *testing.T, environ []string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(t.Context(), append([]string{"consumers"}, args...), environ, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestConsumersAddPrintsTheTokenOnce(t *testing.T) {
	environ := []string{"DATABASE_URL=" + coretest.Database(t)}

	code, out, errOut := runConsumers(t, environ, "add", "notebook")

	lines := strings.Split(out, "\n")
	if code != 0 || len(lines) < 2 || lines[0] != "consumer notebook created; its token, shown only now:" || !regexp.MustCompile(`^mhc_[A-Za-z0-9_-]{43}$`).MatchString(lines[1]) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, list, errOut := runConsumers(t, environ, "list")
	if code != 0 || !strings.Contains(list, "notebook") || !strings.Contains(list, "never") || strings.Contains(list, lines[1]) {
		t.Fatalf("exit %d, list %q, stderr %q", code, list, errOut)
	}
	row := strings.Fields(list[strings.Index(list, "notebook"):])
	if row[1] != "0" || row[2] != "0" {
		t.Errorf("row %q", row)
	}
}

func TestConsumersListWithoutConsumers(t *testing.T) {
	code, out, errOut := runConsumers(t, []string{"DATABASE_URL=" + coretest.Database(t)}, "list")

	if code != 0 || out != "no consumers\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestConsumersRotateAndRevoke(t *testing.T) {
	environ := []string{"DATABASE_URL=" + coretest.Database(t)}
	_, added, _ := runConsumers(t, environ, "add", "notebook")

	code, rotated, errOut := runConsumers(t, environ, "rotate", "notebook")

	first, second := strings.Split(added, "\n")[1], strings.Split(rotated, "\n")[1]
	if code != 0 || !strings.HasPrefix(rotated, "consumer notebook has a new token, shown only now:\n") || first == second {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, rotated, errOut)
	}
	code, out, _ := runConsumers(t, environ, "revoke", "notebook")
	if code != 0 || out != "consumer notebook revoked\n" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	code, out, errOut = runConsumers(t, environ, "revoke", "notebook")
	if code != 1 || !strings.Contains(out+errOut, "no consumer named notebook") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestConsumersAddRefusesADuplicate(t *testing.T) {
	environ := []string{"DATABASE_URL=" + coretest.Database(t)}
	runConsumers(t, environ, "add", "notebook")

	code, out, errOut := runConsumers(t, environ, "add", "notebook")

	if code != 1 || !strings.Contains(out+errOut, "a consumer named notebook already exists") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestConsumersUsageErrors(t *testing.T) {
	usage := "usage: monitor-hub consumers add|rotate|revoke <name> | list\n"
	cases := map[string]struct {
		args []string
		want string
	}{
		"no subcommand":      {nil, usage},
		"unknown subcommand": {[]string{"drop", "x"}, usage},
		"add without a name": {[]string{"add"}, usage},
		"list with a name":   {[]string{"list", "x"}, usage},
		"invalid name":       {[]string{"add", "Bad_Name"}, "consumer names use lowercase letters, digits and single hyphens, 2 to 31 characters, starting with a letter\n"},
	}
	environ := []string{"DATABASE_URL=" + coretest.Database(t)}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runConsumers(t, environ, tc.args...)

			if code != 2 || errOut != tc.want || out != "" {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
		})
	}
}
