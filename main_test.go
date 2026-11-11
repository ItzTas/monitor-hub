package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUnknownCommandListsTheKnownOnes(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(t.Context(), []string{"serv"}, nil, &stdout, &stderr)

	if code != 2 || !strings.Contains(stderr.String(), `unknown command "serv"`) || !strings.Contains(stderr.String(), "serve") || stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestServeStopsOnAnInvalidConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(t.Context(), nil, []string{"API_TOKEN=api-secret-value", "FEED_TOKEN=feed-secret-value"}, &stdout, &stderr)

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
