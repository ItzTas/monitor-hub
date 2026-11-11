package notify

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/internal/core"
)

const botToken = "123456789:AAE-test-token-0123456789abcdefghij"

type telegramPayload struct {
	ChatID      string `json:"chat_id"`
	Text        string `json:"text"`
	LinkPreview struct {
		Disabled bool `json:"is_disabled"`
	} `json:"link_preview_options"`
}

func testTelegram(api string) *telegram {
	return newTelegram(core.Telegram{Token: core.NewSecret(botToken), ChatID: "-1001234567890"}, api, "monitor-hub/test")
}

func TestTelegramSendsTheMessageToTheChat(t *testing.T) {
	server, rec := newServer(t, http.StatusOK, `{"ok":true,"result":{"message_id":1}}`)
	msg := Compose("SEI", []Event{{Plugin: "sei", Kind: "activity", Title: "New activity on process 00000.000001/2026-00", Body: "Process received", OccurredAt: t0}})

	err := testTelegram(server.URL).Send(t.Context(), msg)

	if err != nil {
		t.Fatal(err)
	}
	got := rec.all()[0]
	var payload telegramPayload
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/bot"+botToken+"/sendMessage" || got.header.Get("Content-Type") != "application/json" || got.header.Get("User-Agent") != "monitor-hub/test" {
		t.Errorf("request = %s %s %v", got.method, got.path, got.header)
	}
	want := "monitor-hub: 1 update from SEI\n\nNew activity on process 00000.000001/2026-00\n  Process received"
	if payload.ChatID != "-1001234567890" || payload.Text != want || !payload.LinkPreview.Disabled {
		t.Errorf("payload = %s", got.body)
	}
}

func TestTelegramCutsLongTexts(t *testing.T) {
	server, rec := newServer(t, http.StatusOK, `{"ok":true}`)

	err := testTelegram(server.URL).Send(t.Context(), Message{Subject: "s", Body: strings.Repeat("Ofício ", 1000)})

	var payload telegramPayload
	if err != nil || json.Unmarshal(rec.all()[0].body, &payload) != nil || len(payload.Text) > 4000 || !strings.HasSuffix(payload.Text, "\n…") {
		t.Errorf("Send = %v, text of %d bytes", err, len(payload.Text))
	}
}

func TestTelegramErrorsNeverShowTheBotToken(t *testing.T) {
	refused, _ := newServer(t, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	silent, _ := newServer(t, http.StatusBadGateway, "<html>bad gateway</html>")
	tests := map[string]struct {
		api  string
		want string
	}{
		"refused":     {refused.URL, "the server answered 400: Bad Request: chat not found"},
		"no reason":   {silent.URL, "the server answered 502"},
		"unreachable": {unreachableURL(t), "could not reach the server: "},
	}
	for name, tt := range tests {
		err := testTelegram(tt.api).Send(t.Context(), NewTestMessage())
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) || strings.Contains(err.Error(), "AAE-test-token") || strings.Contains(err.Error(), "/bot") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}
