package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/ItzTas/monitor-hub/src/internal/notify"
)

var registered = types.NewJID("550000000001", types.DefaultUserServer)

type delivered struct {
	to   types.JID
	text string
}

type fakeMessenger struct {
	mu         sync.Mutex
	paired     bool
	connected  bool
	loggedIn   bool
	connectErr error
	hang       chan struct{}
	connects   int
	waits      int
	numbers    map[string]types.JID
	queried    [][]string
	sendErr    error
	stall      bool
	sent       []delivered
}

func (f *fakeMessenger) Paired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paired
}

func (f *fakeMessenger) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeMessenger) ConnectContext(context.Context) error {
	f.mu.Lock()
	f.connects++
	hang := f.hang
	f.mu.Unlock()
	if hang != nil {
		<-hang
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connected = true
	return nil
}

func (f *fakeMessenger) WaitForConnection(time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits++
	return f.connected && f.loggedIn
}

func (f *fakeMessenger) IsOnWhatsApp(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queried = append(f.queried, phones)
	answers := make([]types.IsOnWhatsAppResponse, 0, len(phones))
	for _, phone := range phones {
		jid, ok := f.numbers[phone]
		answers = append(answers, types.IsOnWhatsAppResponse{Query: phone, JID: jid, IsIn: ok})
	}
	return answers, nil
}

func (f *fakeMessenger) SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	stall, sendErr := f.stall, f.sendErr
	f.mu.Unlock()
	if stall {
		<-ctx.Done()
		return whatsmeow.SendResponse{}, fmt.Errorf("failed to send to %s: %w", to, ctx.Err())
	}
	if sendErr != nil {
		return whatsmeow.SendResponse{}, fmt.Errorf("failed to get LID for PN %s: %w", to, sendErr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, delivered{to: to, text: message.GetConversation()})
	return whatsmeow.SendResponse{}, nil
}

func online() *fakeMessenger {
	return &fakeMessenger{paired: true, connected: true, loggedIn: true, numbers: map[string]types.JID{"+" + number: registered}}
}

func TestConnectsOnDemand(t *testing.T) {
	tests := map[string]struct {
		messenger *fakeMessenger
		want      string
	}{
		"reconnects":      {&fakeMessenger{paired: true, loggedIn: true, numbers: map[string]types.JID{"+" + number: registered}}, ""},
		"not paired":      {&fakeMessenger{connectErr: ErrNotPaired}, "could not connect to WhatsApp: not paired with WhatsApp; run monitor-hub whatsapp-pair"},
		"login times out": {&fakeMessenger{paired: true}, "the WhatsApp session did not log in within 30s"},
	}
	for name, tt := range tests {
		err := NewChannel(NewWhatsmeow(tt.messenger), []string{number}, newLedger()).Send(t.Context(), notify.NewTestMessage())

		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tt.want || tt.messenger.connects != 1 {
			t.Errorf("%s: Send = %q, %d connects", name, got, tt.messenger.connects)
		}
		if tt.want != "" && len(tt.messenger.sent) != 0 {
			t.Errorf("%s: sent %d messages", name, len(tt.messenger.sent))
		}
	}
	if !errors.Is(NewChannel(NewWhatsmeow(&fakeMessenger{connectErr: ErrNotPaired}), nil, newLedger()).Send(t.Context(), notify.NewTestMessage()), ErrNotPaired) {
		t.Error("the not paired error does not unwrap to ErrNotPaired")
	}
}

func TestReadyOnlyWithAPairedSession(t *testing.T) {
	for _, paired := range []bool{true, false} {
		if ready := NewWhatsmeow(&fakeMessenger{paired: paired}).Ready(t.Context()); ready != paired {
			t.Errorf("paired %v: Ready = %v", paired, ready)
		}
	}
}

func TestConnectReturnsWhenTheContextEnds(t *testing.T) {
	hung := &fakeMessenger{paired: true, hang: make(chan struct{})}
	t.Cleanup(func() { close(hung.hang) })
	channel := NewChannel(NewWhatsmeow(hung), []string{number}, newLedger())
	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)

		err := channel.Send(ctx, notify.NewTestMessage())

		cancel()
		if err == nil || err.Error() != "could not connect to WhatsApp: context deadline exceeded" {
			t.Errorf("attempt %d: Send = %v", attempt, err)
		}
	}
	hung.mu.Lock()
	defer hung.mu.Unlock()
	if hung.connects != 1 {
		t.Errorf("a hung connection was tried %d times at once", hung.connects)
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
		gateway := NewWhatsmeow(tt.messenger)
		gateway.now = func() time.Time { return now }
		channel := NewChannel(gateway, []string{number}, newLedger())
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
			t.Errorf("%s: after the window Send = %v, %d messenger calls, sent %d messages", name, third, calls(), len(tt.messenger.sent))
		}
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
		gateway := NewWhatsmeow(tt.messenger)
		gateway.loginTimeout = 20 * time.Millisecond
		channel := NewChannel(gateway, []string{number}, newLedger())
		channel.sendTimeout = 20 * time.Millisecond

		err := sendWithin(t, t.Context(), channel)

		if err == nil || err.Error() != tt.want {
			t.Errorf("%s: Send = %v, want %q", name, err, tt.want)
		}
	}
}

func TestWhatsmeowLooksNumbersUpWithAPlus(t *testing.T) {
	messenger := online()
	gateway := NewWhatsmeow(messenger)

	chat, err := gateway.Lookup(t.Context(), number)
	_, offErr := gateway.Lookup(t.Context(), other)

	if err != nil || chat != numberChat {
		t.Errorf("Lookup = %s, the chat is the JID WhatsApp knows: %v", redact(fmt.Sprint(err)), chat == numberChat)
	}
	if !errors.Is(offErr, ErrNotOnWhatsApp) {
		t.Errorf("Lookup of a number off WhatsApp = %s", redact(fmt.Sprint(offErr)))
	}
	want := [][]string{{"+" + number}, {"+" + other}}
	if !slices.EqualFunc(messenger.queried, want, slices.Equal) {
		t.Errorf("WhatsApp was asked %d times, each number with a plus: %v", len(messenger.queried), slices.EqualFunc(messenger.queried, want, slices.Equal))
	}
}

func TestWhatsmeowSendsTheTextToTheChat(t *testing.T) {
	messenger := online()
	gateway := NewWhatsmeow(messenger)
	groupJID := types.NewJID("120363000000000001", types.GroupServer)

	errs := errors.Join(gateway.SendText(t.Context(), numberChat, "hello"), gateway.SendText(t.Context(), group, "hi all"))

	want := []delivered{{registered, "hello"}, {groupJID, "hi all"}}
	if errs != nil || !slices.Equal(messenger.sent, want) {
		t.Errorf("SendText = %s, sent %d messages, as expected: %v", redact(fmt.Sprint(errs)), len(messenger.sent), slices.Equal(messenger.sent, want))
	}
}
