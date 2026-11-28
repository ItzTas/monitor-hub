package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	batchSize      = 500
	pollInterval   = 30 * time.Second
	maxErrorLength = 500
	recordTimeout  = 10 * time.Second
)

var retryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

type Channel interface {
	Name() string
	Send(ctx context.Context, msg Message) error
}

type gate interface {
	Ready(ctx context.Context) bool
}

type Dispatcher struct {
	outbox     Outbox
	channels   []Channel
	log        *slog.Logger
	now        func() time.Time
	after      func(time.Duration) <-chan time.Time
	mu         sync.Mutex
	unrecorded map[string][]Delivery
	wake       chan struct{}
}

type Option func(*Dispatcher)

func WithClock(now func() time.Time, after func(time.Duration) <-chan time.Time) Option {
	return func(d *Dispatcher) {
		d.now, d.after = now, after
	}
}

func NewDispatcher(outbox Outbox, channels []Channel, log *slog.Logger, options ...Option) *Dispatcher {
	d := &Dispatcher{outbox: outbox, channels: channels, log: log, now: time.Now, after: time.After, unrecorded: map[string][]Delivery{}, wake: make(chan struct{}, 1)}
	for _, option := range options {
		option(d)
	}
	return d
}

func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) Run(ctx context.Context) error {
	if len(d.channels) == 0 {
		return nil
	}
	for {
		d.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-d.wake:
		case <-d.after(pollInterval):
		}
	}
}

func (d *Dispatcher) RunOnce(ctx context.Context) {
	var wg sync.WaitGroup
	for _, channel := range d.channels {
		wg.Go(func() { d.deliver(ctx, channel) })
	}
	wg.Wait()
}

func (d *Dispatcher) deliver(ctx context.Context, channel Channel) {
	if !d.recordHeld(ctx, channel.Name()) {
		return
	}
	if gated, ok := channel.(gate); ok && !gated.Ready(ctx) {
		return
	}
	due, err := d.outbox.DueDeliveries(ctx, channel.Name(), d.now(), batchSize)
	if err != nil {
		d.log.Error("could not read the pending notifications", "channel", channel.Name(), "error", err)
		return
	}
	for _, group := range byMonitor(due) {
		if ctx.Err() != nil {
			return
		}
		if !d.send(ctx, channel, group) {
			return
		}
	}
}

func (d *Dispatcher) recordHeld(ctx context.Context, channel string) bool {
	d.mu.Lock()
	held := d.unrecorded[channel]
	d.mu.Unlock()
	if len(held) == 0 {
		return true
	}
	if err := d.record(ctx, held); err != nil {
		d.log.Error("could not record earlier notification attempts; the channel waits", "channel", channel, "deliveries", len(held), "error", err)
		return false
	}
	d.mu.Lock()
	d.unrecorded[channel] = d.unrecorded[channel][len(held):]
	d.mu.Unlock()
	return true
}

func (d *Dispatcher) hold(channel string, attempts []Delivery) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unrecorded[channel] = append(d.unrecorded[channel], attempts...)
}

func (d *Dispatcher) record(ctx context.Context, attempts []Delivery) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return d.outbox.UpdateDeliveries(recordCtx, attempts)
}

func byMonitor(due []DueDelivery) [][]DueDelivery {
	index := map[string]int{}
	var groups [][]DueDelivery
	for _, delivery := range due {
		i, seen := index[delivery.Event.Monitor]
		if !seen {
			i = len(groups)
			index[delivery.Event.Monitor] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], delivery)
	}
	return groups
}

func (d *Dispatcher) send(ctx context.Context, channel Channel, group []DueDelivery) bool {
	events := make([]Event, 0, len(group))
	for _, delivery := range group {
		events = append(events, delivery.Event)
	}
	monitor := group[0].Event.Monitor
	sendErr := channel.Send(ctx, Compose(group[0].Event.DisplayName, events))
	if sendErr != nil && ctx.Err() != nil {
		return false
	}
	now := d.now()
	errFor := outcomes(sendErr)
	attempts := make([]Delivery, 0, len(group))
	for _, delivery := range group {
		attempts = append(attempts, attempt(delivery.Delivery, errFor(delivery.EventSeq), now))
	}
	if err := d.record(ctx, attempts); err != nil {
		d.hold(channel.Name(), attempts)
		d.log.Error("could not record the notification attempt; the channel waits until it can", "channel", channel.Name(), "monitor", monitor, "error", err)
		return false
	}
	d.report(channel.Name(), monitor, attempts, sendErr)
	return true
}

// outcomes gives each event of a send its own error: nil for the events a
// *PartialError lists as done, sendErr for the others.
func outcomes(sendErr error) func(seq int64) error {
	var partial *PartialError
	if !errors.As(sendErr, &partial) {
		return func(int64) error { return sendErr }
	}
	done := map[int64]bool{}
	for _, seq := range partial.Done {
		done[seq] = true
	}
	return func(seq int64) error {
		if done[seq] {
			return nil
		}
		return sendErr
	}
}

func (d *Dispatcher) report(channel, monitor string, attempts []Delivery, sendErr error) {
	if sendErr == nil {
		d.log.Info("notifications sent", "channel", channel, "monitor", monitor, "events", len(attempts))
		return
	}
	gaveUp := 0
	for _, a := range attempts {
		if a.Status == deliveryFailed {
			gaveUp++
		}
	}
	d.log.Warn("could not send the notifications", "channel", channel, "monitor", monitor, "events", len(attempts), "gave_up", gaveUp, "error", sendErr)
}

func attempt(delivery Delivery, sendErr error, now time.Time) Delivery {
	delivery.Attempts++
	if sendErr == nil {
		delivery.Status, delivery.SentAt, delivery.LastError = deliverySent, &now, ""
		return delivery
	}
	delivery.LastError = cut(strings.ReplaceAll(strings.ToValidUTF8(sendErr.Error(), ""), "\x00", ""), maxErrorLength)
	if int(delivery.Attempts) > len(retryDelays) {
		delivery.Status = deliveryFailed
		return delivery
	}
	delivery.NextAttemptAt = now.Add(retryDelays[delivery.Attempts-1])
	return delivery
}
