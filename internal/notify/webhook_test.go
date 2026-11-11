package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/internal/core"
)

const webhookSecret = "0123456789abcdef0123456789abcdef"

func testWebhook(url string) *webhook {
	return newWebhook(core.Webhook{URL: url, Secret: core.NewSecret(webhookSecret)}, "monitor-hub/test")
}

func TestWebhookPostsTheEventsWithAnHMACSignature(t *testing.T) {
	server, rec := newServer(t, http.StatusNoContent, "")
	msg := Compose("SEI", []Event{
		{Plugin: "sei", DisplayName: "SEI", Kind: "return_overdue", Title: "Return overdue", Body: "Process 1", URL: "https://sei.example.org/1", Urgent: true, OccurredAt: t0},
		{Plugin: "sei", DisplayName: "SEI", Kind: "activity", Title: "New activity", OccurredAt: t0},
	})

	err := testWebhook(server.URL+"/hub?key=abc").Send(t.Context(), msg)

	requests := rec.all()
	if err != nil || len(requests) != 1 {
		t.Fatalf("Send = %v, %d requests", err, len(requests))
	}
	got := requests[0]
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(got.body)
	if got.header.Get(signatureHeader) != "sha256="+hex.EncodeToString(mac.Sum(nil)) || signatureHeader != "X-Monitor-Hub-Signature" {
		t.Errorf("signature %s = %q", signatureHeader, got.header.Get(signatureHeader))
	}
	if got.method != http.MethodPost || got.path != "/hub" || got.header.Get("Content-Type") != "application/json" || got.header.Get("User-Agent") != "monitor-hub/test" {
		t.Errorf("request = %s %s %v", got.method, got.path, got.header)
	}
	var payload struct {
		Test   *bool   `json:"test"`
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Test != nil || !reflect.DeepEqual(payload.Events, msg.Events) {
		t.Errorf("payload = %s", got.body)
	}
	wantFirst := `{"plugin":"sei","display_name":"SEI","kind":"return_overdue","title":"Return overdue","body":"Process 1","url":"https://sei.example.org/1","urgent":true,"occurred_at":"2026-11-10T18:00:00Z"}`
	if !strings.Contains(string(got.body), `"events":[`+wantFirst+`,`) {
		t.Errorf("body = %s, want the first event as %s", got.body, wantFirst)
	}
}

func TestWebhookMarksTheTestMessage(t *testing.T) {
	server, rec := newServer(t, http.StatusOK, "")

	err := testWebhook(server.URL).Send(t.Context(), NewTestMessage())

	if err != nil || string(rec.all()[0].body) != `{"test":true,"events":[]}` {
		t.Errorf("Send = %v, body %s", err, rec.all()[0].body)
	}
}

func TestWebhookErrorsLeaveTheAddressOut(t *testing.T) {
	target, targetRequests := newServer(t, http.StatusOK, "")
	failing, _ := newServer(t, http.StatusInternalServerError, "internal error: key=s3cr3t")
	redirecting, _ := newServer(t, http.StatusFound, "", "Location", target.URL+"/elsewhere")
	tests := map[string]struct {
		url  string
		want string
	}{
		"server error": {failing.URL, "the server answered 500"},
		"redirect":     {redirecting.URL, "the server answered 302"},
		"unreachable":  {unreachableURL(t), "could not reach the server: "},
	}
	for name, tt := range tests {
		err := testWebhook(tt.url+"/hook/s3cr3t?key=s3cr3t").Send(t.Context(), NewTestMessage())
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) || strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "/hook") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	if requests := targetRequests.all(); len(requests) != 0 {
		t.Errorf("the client followed the redirect: %+v", requests)
	}
}
