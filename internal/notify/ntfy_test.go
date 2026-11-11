package notify

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ItzTas/monitor-hub/internal/core"
)

func TestNtfyPostsTheTextWithTitleTagsAndPriority(t *testing.T) {
	tests := []struct {
		name          string
		event         Event
		token         string
		priority      string
		authorization string
	}{
		{"routine, public topic", Event{Plugin: "sei", Kind: "activity", Title: "New activity", OccurredAt: t0}, "", "default", ""},
		{"urgent, protected topic", Event{Plugin: "sei", Kind: "return_overdue", Title: "Return overdue", Urgent: true, OccurredAt: t0}, "tk_0123456789", "high", "Bearer tk_0123456789"},
	}
	for _, tt := range tests {
		server, rec := newServer(t, http.StatusOK, `{"id":"abc"}`)
		msg := Compose("SEI", []Event{tt.event})

		err := newNtfy(core.Ntfy{URL: server.URL + "/hub-topic", Token: core.NewSecret(tt.token)}, "monitor-hub/test").Send(t.Context(), msg)

		if err != nil {
			t.Fatalf("%s: Send = %v", tt.name, err)
		}
		got := rec.all()[0]
		header := got.header
		if got.path != "/hub-topic" || string(got.body) != msg.Body || header.Get("Title") != "monitor-hub: 1 update from SEI" ||
			header.Get("Tags") != "monitor-hub" || header.Get("Priority") != tt.priority || header.Get("Authorization") != tt.authorization ||
			header.Get("User-Agent") != "monitor-hub/test" {
			t.Errorf("%s: request %s %v %q", tt.name, got.path, header, got.body)
		}
	}
}

func TestNtfyCutsLongTextsAtACharacter(t *testing.T) {
	server, rec := newServer(t, http.StatusOK, "")

	err := newNtfy(core.Ntfy{URL: server.URL + "/hub-topic"}, "monitor-hub/test").Send(t.Context(), Message{Subject: "s", Body: strings.Repeat("Ofício ", 1000)})

	body := string(rec.all()[0].body)
	if err != nil || len(body) > 4000 || !utf8.ValidString(body) || !strings.HasSuffix(body, "\n…") || !strings.HasPrefix(body, "Ofício Ofício") {
		t.Errorf("Send = %v, %d bytes, ends with %q", err, len(body), body[max(0, len(body)-12):])
	}
}

func TestNtfyErrorsLeaveTheTokenAndAddressOut(t *testing.T) {
	failing, _ := newServer(t, http.StatusUnauthorized, "token tk_s3cr3t rejected")
	tests := map[string]struct {
		url  string
		want string
	}{
		"refused":     {failing.URL, "the server answered 401"},
		"unreachable": {unreachableURL(t), "could not reach the server: "},
	}
	for name, tt := range tests {
		err := newNtfy(core.Ntfy{URL: tt.url + "/topic-s3cr3t", Token: core.NewSecret("tk_s3cr3t")}, "monitor-hub/test").Send(t.Context(), NewTestMessage())
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}
