package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/notify"
)

const (
	group      = "120363000000000001@g.us"
	number     = "5500900000001"
	other      = "5500900000002"
	numberChat = "550000000001@s.whatsapp.net"
)

type sentText struct {
	chat string
	text string
}

type fakeGateway struct {
	ready      bool
	readyCtx   context.Context
	connectErr error
	connects   int
	chats      map[string]string
	lookups    int
	sendErr    error
	stall      bool
	sent       []sentText
}

func (f *fakeGateway) Ready(ctx context.Context) bool {
	f.readyCtx = ctx
	return f.ready
}

func (f *fakeGateway) Connect(context.Context) error {
	f.connects++
	return f.connectErr
}

func (f *fakeGateway) Lookup(_ context.Context, number string) (string, error) {
	f.lookups++
	chat, ok := f.chats[number]
	if !ok {
		return "", ErrNotOnWhatsApp
	}
	return chat, nil
}

func (f *fakeGateway) SendText(ctx context.Context, chat, text string) error {
	if f.stall {
		<-ctx.Done()
		return fmt.Errorf("failed to send to %s: %w", chat, ctx.Err())
	}
	if f.sendErr != nil {
		return fmt.Errorf("failed to send to %s: %w", chat, f.sendErr)
	}
	f.sent = append(f.sent, sentText{chat: chat, text: text})
	return nil
}

func sendWithin(t *testing.T, ctx context.Context, channel *Channel) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- channel.Send(ctx, notify.NewTestMessage()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Send still running after 5s")
		return nil
	}
}

func gateway() *fakeGateway {
	return &fakeGateway{ready: true, chats: map[string]string{number: numberChat}}
}

func TestSendsToTheNumberWhatsAppKnowsAndToGroups(t *testing.T) {
	gw := gateway()
	channel := NewChannel(gw, []string{number, group})
	msg := notify.Message{Subject: "1 update from SEI", Body: "New document on process 00000.000001/2026-00"}

	first := channel.Send(t.Context(), msg)
	second := channel.Send(t.Context(), msg)

	if first != nil || second != nil {
		t.Fatalf("Send = %v, %v", first, second)
	}
	text := "monitor-hub: 1 update from SEI\n\nNew document on process 00000.000001/2026-00"
	want := []sentText{{numberChat, text}, {group, text}, {numberChat, text}, {group, text}}
	if !slices.Equal(gw.sent, want) || gw.lookups != 1 || gw.connects != 2 {
		t.Errorf("sent %d messages (as expected: %v), %d lookups, %d connects", len(gw.sent), slices.Equal(gw.sent, want), gw.lookups, gw.connects)
	}
}

func TestANumberOffWhatsAppDoesNotStopTheOtherRecipients(t *testing.T) {
	gw := gateway()
	channel := NewChannel(gw, []string{other, group})

	err := channel.Send(t.Context(), notify.NewTestMessage())

	if err == nil || err.Error() != "recipient 1 of 2: the number is not on WhatsApp" || !errors.Is(err, ErrNotOnWhatsApp) {
		t.Errorf("Send = %v", err)
	}
	if len(gw.sent) != 1 || gw.sent[0].chat != group {
		t.Errorf("sent %d messages, the group got one: %v", len(gw.sent), len(gw.sent) == 1 && gw.sent[0].chat == group)
	}
}

func TestFailedSendErrorsShowNoPhoneNumber(t *testing.T) {
	failing := gateway()
	failing.sendErr = errors.New("server returned error 500")
	refused := gateway()
	refused.connectErr = fmt.Errorf("noise handshake failed for %s:7@s.whatsapp.net", number)
	tests := map[string]struct {
		gateway *fakeGateway
		want    string
	}{
		"send":    {failing, "recipient 1 of 1: failed to send to [redacted]@s.whatsapp.net: server returned error 500"},
		"connect": {refused, "noise handshake failed for [redacted]:7@s.whatsapp.net"},
	}
	for name, tt := range tests {
		err := NewChannel(tt.gateway, []string{number}).Send(t.Context(), notify.NewTestMessage())

		if err == nil {
			t.Errorf("%s: Send succeeded", name)
			continue
		}
		if strings.Contains(err.Error(), number) || strings.Contains(err.Error(), strings.TrimSuffix(numberChat, "@s.whatsapp.net")) {
			t.Errorf("%s: the error shows a phone number", name)
			continue
		}
		if err.Error() != tt.want {
			t.Errorf("%s: Send = %v, want %q", name, err, tt.want)
		}
	}
}

func TestSendReturnsWhenTheContextEnds(t *testing.T) {
	gw := gateway()
	gw.stall = true
	channel := NewChannel(gw, []string{number})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := sendWithin(t, ctx, channel)

	want := "recipient 1 of 1: failed to send to [redacted]@s.whatsapp.net: context deadline exceeded"
	if err == nil || err.Error() != want {
		t.Errorf("Send = %v, want %q", err, want)
	}
}

func TestReadyAsksTheGateway(t *testing.T) {
	type key struct{}
	for _, ready := range []bool{true, false} {
		gw := &fakeGateway{ready: ready}
		ctx := context.WithValue(t.Context(), key{}, ready)

		got := NewChannel(gw, []string{number}).Ready(ctx)

		if got != ready || gw.readyCtx != ctx {
			t.Errorf("gateway ready %v: Ready = %v, got the caller's context: %v", ready, got, gw.readyCtx == ctx)
		}
	}
}

func TestUnavailableChannelNeverSends(t *testing.T) {
	channel := Unavailable{}

	err := channel.Send(t.Context(), notify.NewTestMessage())

	want := "the WhatsApp session could not be opened when monitor-hub started; restart monitor-hub once the session store works"
	if channel.Name() != "whatsapp" || channel.Ready(t.Context()) || err == nil || err.Error() != want {
		t.Errorf("name %q, ready %v, Send = %v", channel.Name(), channel.Ready(t.Context()), err)
	}
}
