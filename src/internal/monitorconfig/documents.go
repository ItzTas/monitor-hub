package monitorconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

const readTimeout = 5 * time.Second

// Documents prepares each start of a monitor. When the hub's database fails, a monitor starts
// with the configuration last read for it.
type Documents struct {
	entries  map[string]catalog.Entry
	environ  []string
	dataRoot string
	store    Store
	log      *slog.Logger

	mu   sync.Mutex
	last map[string]Saved
}

func NewDocuments(entries []catalog.Entry, environ []string, dataRoot string, store Store, log *slog.Logger) *Documents {
	byName := make(map[string]catalog.Entry, len(entries))
	for _, entry := range entries {
		byName[entry.Manifest.Name] = entry
	}
	return &Documents{entries: byName, environ: environ, dataRoot: dataRoot, store: store, log: log, last: map[string]Saved{}}
}

func (d *Documents) Prepare(ctx context.Context, name string) (supervisor.Delivery, error) {
	entry, ok := d.entries[name]
	if !ok {
		return supervisor.Delivery{}, fmt.Errorf("monitor %s is not in the catalog", name)
	}
	if entry.Settings == nil {
		return supervisor.Delivery{Env: catalog.Environment(name, d.environ, d.dataRoot, catalog.Layer{})}, nil
	}
	saved, err := d.saved(ctx, name)
	if err != nil {
		return supervisor.Delivery{}, err
	}
	a := Assemble(entry, d.environ, d.dataRoot, saved)
	if len(a.Problems) > 0 {
		return supervisor.Delivery{}, problemsError(a.Problems)
	}
	return supervisor.Delivery{Env: a.Env, Document: a.Document}, nil
}

func (d *Documents) saved(ctx context.Context, name string) (Saved, error) {
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	saved, err := d.store.Get(readCtx, name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		d.last[name] = clone(saved)
		return saved, nil
	}
	if ctx.Err() != nil {
		return Saved{}, ctx.Err()
	}
	last, ok := d.last[name]
	if !ok {
		return Saved{}, fmt.Errorf("read the saved configuration: %w", err)
	}
	d.log.Warn("the hub's database is unreachable; starting the monitor with the configuration it last read", "monitor", name, "error", err)
	return clone(last), nil
}

func clone(saved Saved) Saved {
	saved.Document = maps.Clone(saved.Document)
	saved.Secrets = maps.Clone(saved.Secrets)
	saved.Ignored = slices.Clone(saved.Ignored)
	saved.Variables = maps.Clone(saved.Variables)
	return saved
}

func problemsError(problems []Problem) error {
	texts := make([]string, len(problems))
	for i, p := range problems {
		texts[i] = p.Message
		if p.Path != "" {
			texts[i] = p.Path + ": " + p.Message
		}
	}
	return errors.New(strings.Join(texts, "; "))
}
