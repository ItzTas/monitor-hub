package whatsapp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/notify"
)

const (
	group      = "120363000000000001@g.us"
	number     = "5500900000001"
	other      = "5500900000002"
	numberChat = "550000000001@s.whatsapp.net"
	otherChat  = other + "@s.whatsapp.net"
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
	failOnce   map[string]error
	onSend     func()
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
	if err, fails := f.failOnce[chat]; fails {
		delete(f.failOnce, chat)
		return fmt.Errorf("failed to send to %s: %w", chat, err)
	}
	if f.onSend != nil {
		f.onSend()
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
	return &fakeGateway{ready: true, chats: map[string]string{number: numberChat, other: otherChat}}
}

func TestSendsToTheNumberWhatsAppKnowsAndToGroups(t *testing.T) {
	gw := gateway()
	channel := NewChannel(gw, []string{number, group}, newLedger())
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
	delete(gw.chats, other)
	channel := NewChannel(gw, []string{other, group}, newLedger())

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
		err := NewChannel(tt.gateway, []string{number}, newLedger()).Send(t.Context(), notify.NewTestMessage())

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
	channel := NewChannel(gw, []string{number}, newLedger())
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

		got := NewChannel(gw, []string{number}, newLedger()).Ready(ctx)

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

var (
	event1 = notify.Event{Seq: 1, Monitor: "sei", DisplayName: "SEI", Kind: "document", Title: "New document on process 00000.000001/2026-00"}
	event2 = notify.Event{Seq: 2, Monitor: "sei", DisplayName: "SEI", Kind: "document", Title: "New document on process 00000.000002/2026-00"}
)

type memLedger struct {
	mu         sync.Mutex
	reached    map[string]map[int64]bool
	readErr    error
	recordErr  error
	reads      int
	records    int
	recordCtxs []error
}

func newLedger() *memLedger {
	return &memLedger{reached: map[string]map[int64]bool{}}
}

func (l *memLedger) Reached(_ context.Context, seqs []int64) (map[string]map[int64]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if l.readErr != nil {
		return nil, l.readErr
	}
	reached := map[string]map[int64]bool{}
	for recipient, got := range l.reached {
		for _, seq := range seqs {
			if !got[seq] {
				continue
			}
			if reached[recipient] == nil {
				reached[recipient] = map[int64]bool{}
			}
			reached[recipient][seq] = true
		}
	}
	return reached, nil
}

func (l *memLedger) Record(ctx context.Context, recipient string, seqs []int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records++
	l.recordCtxs = append(l.recordCtxs, ctx.Err())
	if err := cmp.Or(l.recordErr, ctx.Err()); err != nil {
		return err
	}
	l.add(recipient, seqs...)
	return nil
}

func (l *memLedger) add(recipient string, seqs ...int64) {
	if l.reached[recipient] == nil {
		l.reached[recipient] = map[int64]bool{}
	}
	for _, seq := range seqs {
		l.reached[recipient][seq] = true
	}
}

func (l *memLedger) got(recipient string) []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(maps.Keys(l.reached[recipient]))
}

func updates() notify.Message {
	return notify.Compose("SEI", []notify.Event{event1, event2})
}

func textOf(msg notify.Message) string {
	return "monitor-hub: " + msg.Subject + "\n\n" + msg.Body
}

func chatsOf(sent []sentText) []string {
	chats := make([]string, 0, len(sent))
	for _, s := range sent {
		chats = append(chats, s.chat)
	}
	return chats
}

func TestARetrySendsOnlyToRecipientsThatMissedTheMessage(t *testing.T) {
	gw := gateway()
	gw.failOnce = map[string]error{otherChat: errors.New("server returned error 500")}
	ledger := newLedger()
	recipients := []string{number, other, group}
	channel := NewChannel(gw, recipients, ledger)

	first := channel.Send(t.Context(), updates())
	firstChats := chatsOf(gw.sent)
	gw.sent = nil
	second := channel.Send(t.Context(), updates())

	want := "recipient 2 of 3: failed to send to [redacted]@s.whatsapp.net: server returned error 500"
	if first == nil || first.Error() != want || second != nil {
		t.Errorf("Send = %v, then %v; want %q, then nil", first, second, want)
	}
	if !slices.Equal(firstChats, []string{numberChat, group}) || !slices.Equal(chatsOf(gw.sent), []string{otherChat}) {
		t.Errorf("the first send reached %d chats and the retry %d; as expected: %v, %v", len(firstChats), len(gw.sent), slices.Equal(firstChats, []string{numberChat, group}), slices.Equal(chatsOf(gw.sent), []string{otherChat}))
	}
	for i, recipient := range recipients {
		if got := ledger.got(recipient); !slices.Equal(got, []int64{1, 2}) {
			t.Errorf("recipient %d is recorded for events %v; want [1 2]", i+1, got)
		}
	}
}

func TestEachRecipientGetsOnlyTheEventsItMissed(t *testing.T) {
	gw := gateway()
	ledger := newLedger()
	ledger.add(number, 1)
	msg := updates()

	err := NewChannel(gw, []string{number, other}, ledger).Send(t.Context(), msg)

	want := []sentText{{numberChat, "monitor-hub: 1 update from SEI\n\n" + event2.Title}, {otherChat, textOf(msg)}}
	if err != nil || !slices.Equal(gw.sent, want) {
		t.Errorf("Send = %v, sent %d messages, as expected: %v", err, len(gw.sent), slices.Equal(gw.sent, want))
	}
	if got := ledger.got(number); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("recipient 1 is recorded for events %v; want [1 2]", got)
	}
}

func TestTheRecipientListCanChangeBetweenAttempts(t *testing.T) {
	gw := gateway()
	ledger := newLedger()
	ledger.add("5500900000009", 1)
	ledger.add(number, 1)
	msg := notify.Compose("SEI", []notify.Event{event1})

	err := NewChannel(gw, []string{number, other}, ledger).Send(t.Context(), msg)

	want := []sentText{{otherChat, textOf(msg)}}
	if err != nil || !slices.Equal(gw.sent, want) {
		t.Errorf("Send = %v, sent %d messages, only to the new recipient: %v", err, len(gw.sent), slices.Equal(gw.sent, want))
	}
}

func TestAFailedRecordStopsTheSendAndRetriesThatRecipient(t *testing.T) {
	gw := gateway()
	ledger := newLedger()
	ledger.recordErr = errors.New("connection refused")
	channel := NewChannel(gw, []string{number, other}, ledger)

	first := channel.Send(t.Context(), updates())
	firstChats := chatsOf(gw.sent)
	ledger.recordErr = nil
	second := channel.Send(t.Context(), updates())

	want := "recipient 1 of 2 got the message, but monitor-hub could not record it, so a retry may send it again: connection refused"
	if first == nil || first.Error() != want || second != nil {
		t.Errorf("Send = %v, then %v; want %q, then nil", first, second, want)
	}
	if !slices.Equal(firstChats, []string{numberChat}) || !slices.Equal(chatsOf(gw.sent), []string{numberChat, numberChat, otherChat}) {
		t.Errorf("the first send reached %d chats and both sends %d; as expected: %v, %v", len(firstChats), len(gw.sent), slices.Equal(firstChats, []string{numberChat}), slices.Equal(chatsOf(gw.sent), []string{numberChat, numberChat, otherChat}))
	}
}

func TestAnUnreadableLedgerSendsNothing(t *testing.T) {
	gw := gateway()
	ledger := newLedger()
	ledger.readErr = errors.New("connection refused")

	err := NewChannel(gw, []string{number, group}, ledger).Send(t.Context(), updates())

	want := "could not read which recipients already got these notifications: connection refused"
	if err == nil || err.Error() != want || len(gw.sent) != 0 || gw.connects != 0 {
		t.Errorf("Send = %v, sent %d messages, %d connects", err, len(gw.sent), gw.connects)
	}
}

func TestTheTestMessageGoesToEveryoneAndIsNotRecorded(t *testing.T) {
	gw := gateway()
	ledger := newLedger()

	err := NewChannel(gw, []string{number, other, group}, ledger).Send(t.Context(), notify.NewTestMessage())

	if err != nil || !slices.Equal(chatsOf(gw.sent), []string{numberChat, otherChat, group}) || ledger.reads != 0 || ledger.records != 0 {
		t.Errorf("Send = %v, sent %d messages, %d ledger reads, %d records", err, len(gw.sent), ledger.reads, ledger.records)
	}
}

func TestTheRecordOutlivesTheSendDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	gw := gateway()
	gw.onSend = cancel
	ledger := newLedger()

	err := NewChannel(gw, []string{number}, ledger).Send(ctx, notify.Compose("SEI", []notify.Event{event1}))

	if err != nil || !slices.Equal(ledger.recordCtxs, []error{nil}) || !slices.Equal(ledger.got(number), []int64{1}) {
		t.Errorf("Send = %v, the record saw context errors %v, recorded events %v", err, ledger.recordCtxs, ledger.got(number))
	}
}

func TestAFailedSendReportsTheEventsEveryoneGot(t *testing.T) {
	gw := gateway()
	gw.failOnce = map[string]error{otherChat: errors.New("server returned error 500")}
	ledger := newLedger()
	ledger.add(other, 1)

	err := NewChannel(gw, []string{number, other}, ledger).Send(t.Context(), updates())

	var partial *notify.PartialError
	want := "recipient 2 of 2: failed to send to [redacted]@s.whatsapp.net: server returned error 500"
	if !errors.As(err, &partial) || !slices.Equal(partial.Done, []int64{1}) || err.Error() != want {
		t.Errorf("Send = %v (%T); want a partial error with event 1 done and %q", err, err, want)
	}
}

func TestNothingToSendNeedsNoConnection(t *testing.T) {
	gw := gateway()
	ledger := newLedger()
	ledger.add(number, 1, 2)
	ledger.add(other, 1, 2)

	err := NewChannel(gw, []string{number, other}, ledger).Send(t.Context(), updates())

	if err != nil || gw.connects != 0 || len(gw.sent) != 0 {
		t.Errorf("Send = %v, %d connects, sent %d messages", err, gw.connects, len(gw.sent))
	}
}
