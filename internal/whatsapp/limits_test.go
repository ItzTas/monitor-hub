package whatsapp

import (
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/ItzTas/monitor-hub/internal/notify"
)

func sendWithin(t *testing.T, channel *Channel, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- channel.Send(t.Context(), notify.NewTestMessage()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("Send still running after %s", limit)
		return nil
	}
}

func TestLimitsEndASendWithoutADeadline(t *testing.T) {
	stalled := online()
	stalled.stall = true
	hung := &fakeMessenger{paired: true, hang: make(chan struct{})}
	t.Cleanup(func() { close(hung.hang) })
	tests := map[string]struct {
		messenger *fakeMessenger
		want      string
	}{
		"login": {hung, "the WhatsApp session did not log in within 20ms"},
		"send":  {stalled, "recipient 1 of 1: failed to send to [redacted]@s.whatsapp.net: context deadline exceeded"},
	}
	for name, tt := range tests {
		channel := NewChannel(tt.messenger, []string{number})
		channel.loginTimeout, channel.sendTimeout = 20*time.Millisecond, 20*time.Millisecond

		err := sendWithin(t, channel, 5*time.Second)

		if err == nil || err.Error() != tt.want {
			t.Errorf("%s: Send = %v, want %q", name, err, tt.want)
		}
	}
}

func TestFailedLoginAnswersAtOnceUntilTheRetryWindowEnds(t *testing.T) {
	tests := map[string]struct {
		messenger *fakeMessenger
		want      string
	}{
		"connect fails":       {&fakeMessenger{paired: true, connectErr: fmt.Errorf("dial wss://web.whatsapp.com for %s: connection refused", number)}, "could not connect to WhatsApp: dial wss://web.whatsapp.com for [redacted]: connection refused"},
		"login never happens": {&fakeMessenger{paired: true}, "the WhatsApp session did not log in within 30s"},
	}
	for name, tt := range tests {
		tt.messenger.numbers = map[string]types.JID{"+" + number: registered}
		now := time.Unix(1_800_000_000, 0)
		channel := NewChannel(tt.messenger, []string{number})
		channel.now = func() time.Time { return now }
		calls := func() int { return tt.messenger.connects + tt.messenger.waits }

		first := channel.Send(t.Context(), notify.NewTestMessage())
		afterFirst := calls()
		now = now.Add(time.Minute - time.Nanosecond)
		second := channel.Send(t.Context(), notify.NewTestMessage())

		if first == nil || second == nil || first.Error() != tt.want || second.Error() != tt.want || calls() != afterFirst {
			t.Errorf("%s: Send = %v, then %v, %d messenger calls after the first send and %d after the second", name, first, second, afterFirst, calls())
		}

		tt.messenger.connectErr, tt.messenger.loggedIn = nil, true
		now = now.Add(time.Nanosecond)
		third := channel.Send(t.Context(), notify.NewTestMessage())

		if third != nil || calls() == afterFirst || len(tt.messenger.sent) != 1 {
			t.Errorf("%s: after the window Send = %v, %d messenger calls, sent %+v", name, third, calls(), tt.messenger.sent)
		}
	}
}
