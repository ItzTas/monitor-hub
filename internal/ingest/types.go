package ingest

import (
	"fmt"
	"time"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

type Clients interface {
	Names() []string
	Client(name string) (pluginpb.PluginClient, bool)
}

type Batch struct {
	Plugin   string
	After    int64
	Events   []*pluginpb.Event
	Channels []string
	Baseline bool
	Now      time.Time
}

func (b Batch) validate() error {
	previous := b.After
	for _, e := range b.Events {
		if e.GetSeq() <= previous {
			return fmt.Errorf("ingest: seq %d follows %d, so the page does not strictly increase after %d", e.GetSeq(), previous, b.After)
		}
		if e.GetId() == "" {
			return fmt.Errorf("ingest: event seq %d has no id", e.GetSeq())
		}
		if !e.GetOccurredAt().IsValid() {
			return fmt.Errorf("ingest: event seq %d has no valid occurred_at", e.GetSeq())
		}
		previous = e.GetSeq()
	}
	return nil
}
