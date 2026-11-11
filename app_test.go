package main

import (
	"context"
	"testing"

	"github.com/ItzTas/monitor-hub/internal/notify"
)

type plainChannel struct{}

func (plainChannel) Name() string {
	return "plain"
}

func (plainChannel) Send(context.Context, notify.Message) error {
	return nil
}

type closableChannel struct {
	plainChannel
	closed int
}

func (c *closableChannel) Close() error {
	c.closed++
	return nil
}

func TestClosersFindsOnlyTheChannelsThatHoldAConnection(t *testing.T) {
	closable := &closableChannel{}

	found := closers([]notify.Channel{plainChannel{}, closable})

	if len(found) != 1 {
		t.Fatalf("closers found %d, want 1", len(found))
	}
	if err := found[0].Close(); err != nil || closable.closed != 1 {
		t.Errorf("Close = %v, closed %d times", err, closable.closed)
	}
}
