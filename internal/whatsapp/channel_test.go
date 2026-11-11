package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/ItzTas/monitor-hub/internal/notify"
)

const (
	group  = "120363012345678901@g.us"
	number = "5595991234567"
)

var registered = types.NewJID("559591234567", types.DefaultUserServer)

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
	lookups    int
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
	f.lookups++
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

func TestSendsToTheNumberWhatsAppKnowsAndToGroups(t *testing.T) {
	messenger := online()
	channel := NewChannel(messenger, []string{number, group})
	msg := notify.Message{Subject: "1 update from SEI", Body: "New document on process 00000.000001/2026-00"}

	first := channel.Send(t.Context(), msg)
	second := channel.Send(t.Context(), msg)

	if first != nil || second != nil {
		t.Fatalf("Send = %v, %v", first, second)
	}
	text := "monitor-hub: 1 update from SEI\n\nNew document on process 00000.000001/2026-00"
	groupJID, _ := types.ParseJID(group)
	want := []delivered{{registered, text}, {groupJID, text}, {registered, text}, {groupJID, text}}
	if !slices.Equal(messenger.sent, want) || messenger.lookups != 1 || messenger.connects != 0 {
		t.Errorf("sent %+v, %d lookups, %d connects", messenger.sent, messenger.lookups, messenger.connects)
	}
}

func TestANumberOffWhatsAppDoesNotStopTheOtherRecipients(t *testing.T) {
	messenger := online()
	channel := NewChannel(messenger, []string{"5595990000000", group})

	err := channel.Send(t.Context(), notify.NewTestMessage())

	if err == nil || err.Error() != "recipient 1 of 2: the number is not on WhatsApp" || len(messenger.sent) != 1 || messenger.sent[0].to.Server != types.GroupServer {
		t.Errorf("Send = %v, sent %+v", err, messenger.sent)
	}
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
		err := NewChannel(tt.messenger, []string{number}).Send(t.Context(), notify.NewTestMessage())

		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tt.want || tt.messenger.connects != 1 {
			t.Errorf("%s: Send = %q, %d connects", name, got, tt.messenger.connects)
		}
		if tt.want != "" && len(tt.messenger.sent) != 0 {
			t.Errorf("%s: sent %+v", name, tt.messenger.sent)
		}
	}
	if !errors.Is(NewChannel(&fakeMessenger{connectErr: ErrNotPaired}, nil).Send(t.Context(), notify.NewTestMessage()), ErrNotPaired) {
		t.Error("the not paired error does not unwrap to ErrNotPaired")
	}
}

func TestReadyOnlyWithAPairedSession(t *testing.T) {
	for _, paired := range []bool{true, false} {
		if ready := NewChannel(&fakeMessenger{paired: paired}, []string{number}).Ready(); ready != paired {
			t.Errorf("paired %v: Ready = %v", paired, ready)
		}
	}
}

func TestUnavailableChannelNeverSends(t *testing.T) {
	channel := Unavailable{}

	err := channel.Send(t.Context(), notify.NewTestMessage())

	want := "the WhatsApp session could not be opened when monitor-hub started; restart monitor-hub once the session store works"
	if channel.Name() != "whatsapp" || channel.Ready() || err == nil || err.Error() != want {
		t.Errorf("name %q, ready %v, Send = %v", channel.Name(), channel.Ready(), err)
	}
}

func TestFailedSendErrorsShowNoPhoneNumber(t *testing.T) {
	failing := online()
	failing.sendErr = errors.New("server returned error 500")
	refused := &fakeMessenger{paired: true, connectErr: fmt.Errorf("noise handshake failed for %s:7@s.whatsapp.net", number)}
	tests := map[string]struct {
		messenger *fakeMessenger
		want      string
	}{
		"send":    {failing, "recipient 1 of 1: failed to get LID for PN [redacted]@s.whatsapp.net: server returned error 500"},
		"connect": {refused, "could not connect to WhatsApp: noise handshake failed for [redacted]:7@s.whatsapp.net"},
	}
	for name, tt := range tests {
		err := NewChannel(tt.messenger, []string{number}).Send(t.Context(), notify.NewTestMessage())

		if err == nil || err.Error() != tt.want {
			t.Errorf("%s: Send = %v, want %q", name, err, tt.want)
		}
		for _, digits := range []string{number, registered.User} {
			if err != nil && strings.Contains(err.Error(), digits) {
				t.Errorf("%s: the error shows %s: %v", name, digits, err)
			}
		}
	}
}

func TestSendReturnsWhenTheContextEnds(t *testing.T) {
	stalled := online()
	stalled.stall = true
	hung := &fakeMessenger{paired: true, hang: make(chan struct{})}
	t.Cleanup(func() { close(hung.hang) })
	tests := map[string]struct {
		messenger *fakeMessenger
		want      string
	}{
		"while sending":    {stalled, "recipient 1 of 1: failed to send to [redacted]@s.whatsapp.net: context deadline exceeded"},
		"while connecting": {hung, "could not connect to WhatsApp: context deadline exceeded"},
	}
	for name, tt := range tests {
		channel := NewChannel(tt.messenger, []string{number})
		for attempt := 1; attempt <= 2; attempt++ {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			started := time.Now()

			err := channel.Send(ctx, notify.NewTestMessage())

			cancel()
			if err == nil || err.Error() != tt.want || time.Since(started) > 5*time.Second {
				t.Errorf("%s, attempt %d: Send = %v after %s", name, attempt, err, time.Since(started))
			}
		}
	}
	hung.mu.Lock()
	defer hung.mu.Unlock()
	if hung.connects != 1 {
		t.Errorf("a hung connection was tried %d times at once", hung.connects)
	}
}
