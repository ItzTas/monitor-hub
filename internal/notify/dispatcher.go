package notify

import (
	"context"
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

type Dispatcher struct {
	outbox   Outbox
	channels []Channel
	log      *slog.Logger
	now      func() time.Time
	after    func(time.Duration) <-chan time.Time
}

type Option func(*Dispatcher)

func WithClock(now func() time.Time, after func(time.Duration) <-chan time.Time) Option {
	return func(d *Dispatcher) {
		d.now, d.after = now, after
	}
}

func NewDispatcher(outbox Outbox, channels []Channel, log *slog.Logger, options ...Option) *Dispatcher {
	d := &Dispatcher{outbox: outbox, channels: channels, log: log, now: time.Now, after: time.After}
	for _, option := range options {
		option(d)
	}
	return d
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
	due, err := d.outbox.DueDeliveries(ctx, channel.Name(), d.now(), batchSize)
	if err != nil {
		d.log.Error("could not read the pending notifications", "channel", channel.Name(), "error", err)
		return
	}
	for _, group := range byPlugin(due) {
		if ctx.Err() != nil {
			return
		}
		d.send(ctx, channel, group)
	}
}

func byPlugin(due []DueDelivery) [][]DueDelivery {
	index := map[string]int{}
	var groups [][]DueDelivery
	for _, delivery := range due {
		i, seen := index[delivery.Event.Plugin]
		if !seen {
			i = len(groups)
			index[delivery.Event.Plugin] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], delivery)
	}
	return groups
}

func (d *Dispatcher) send(ctx context.Context, channel Channel, group []DueDelivery) {
	events := make([]Event, 0, len(group))
	for _, delivery := range group {
		events = append(events, delivery.Event)
	}
	plugin := group[0].Event.Plugin
	sendErr := channel.Send(ctx, Compose(group[0].Event.DisplayName, events))
	if sendErr != nil && ctx.Err() != nil {
		return
	}
	now := d.now()
	attempts := make([]Delivery, 0, len(group))
	for _, delivery := range group {
		attempts = append(attempts, attempt(delivery.Delivery, sendErr, now))
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := d.outbox.UpdateDeliveries(recordCtx, attempts); err != nil {
		d.log.Error("could not record the notification attempt", "channel", channel.Name(), "plugin", plugin, "error", err)
		return
	}
	d.report(channel.Name(), plugin, attempts, sendErr)
}

func (d *Dispatcher) report(channel, plugin string, attempts []Delivery, sendErr error) {
	if sendErr == nil {
		d.log.Info("notifications sent", "channel", channel, "plugin", plugin, "events", len(attempts))
		return
	}
	gaveUp := 0
	for _, a := range attempts {
		if a.Status == deliveryFailed {
			gaveUp++
		}
	}
	d.log.Warn("could not send the notifications", "channel", channel, "plugin", plugin, "events", len(attempts), "gave_up", gaveUp, "error", sendErr)
}

func attempt(delivery Delivery, sendErr error, now time.Time) Delivery {
	delivery.Attempts++
	if sendErr == nil {
		delivery.Status, delivery.SentAt, delivery.LastError = deliverySent, &now, ""
		return delivery
	}
	delivery.LastError = cut(strings.ToValidUTF8(sendErr.Error(), ""), maxErrorLength)
	if int(delivery.Attempts) > len(retryDelays) {
		delivery.Status = deliveryFailed
		return delivery
	}
	delivery.NextAttemptAt = now.Add(retryDelays[delivery.Attempts-1])
	return delivery
}
