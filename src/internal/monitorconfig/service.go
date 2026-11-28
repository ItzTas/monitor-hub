package monitorconfig

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

var (
	ErrUnknown  = errors.New("no monitor has this name")
	ErrDatabase = errors.New("the hub's database cannot be reached")
)

type Supervisor interface {
	Restart(name string) (uint64, error)
	LastAttempt(name string) supervisor.Attempt
	Client(name string) (monitorpb.MonitorClient, bool)
}

type Checker interface {
	Validate(ctx context.Context, entry catalog.Entry, env []string, document []byte) ([]Problem, error)
}

type Deps struct {
	Entries    []catalog.Entry
	Environ    []string
	DataRoot   string
	Store      Store
	Supervisor Supervisor
	Checker    Checker
	Log        *slog.Logger
	Now        func() time.Time
}

// Service reads a monitor's configuration for the API.
type Service struct {
	deps    Deps
	entries map[string]catalog.Entry
}

func NewService(deps Deps) *Service {
	entries := make(map[string]catalog.Entry, len(deps.Entries))
	for _, entry := range deps.Entries {
		entries[entry.Manifest.Name] = entry
	}
	return &Service{deps: deps, entries: entries}
}

// load reads what is saved for a monitor and assembles its document.
func (s *Service) load(ctx context.Context, entry catalog.Entry) (Saved, Assembly, error) {
	saved, err := s.deps.Store.Get(ctx, entry.Manifest.Name)
	if err != nil {
		return Saved{}, Assembly{}, err
	}
	return saved, Assemble(entry, s.deps.Environ, s.deps.DataRoot, saved), nil
}

// WarnIgnored logs, for each monitor, the paths of the saved values that do not reach it. A
// monitor whose configuration cannot be read is logged and skipped.
func (s *Service) WarnIgnored(ctx context.Context) {
	paths := make([][]string, len(s.deps.Entries))
	errs := make([]error, len(s.deps.Entries))
	var g errgroup.Group
	g.SetLimit(8)
	for i, entry := range s.deps.Entries {
		g.Go(func() error {
			_, a, err := s.load(ctx, entry)
			errs[i] = err
			for _, ignored := range a.Ignored {
				paths[i] = append(paths[i], ignored.Path)
			}
			return nil
		})
	}
	_ = g.Wait()
	for i, entry := range s.deps.Entries {
		name := entry.Manifest.Name
		if errs[i] != nil {
			s.deps.Log.Warn("the saved configuration could not be read", "monitor", name, "error", errs[i])
			continue
		}
		if len(paths[i]) > 0 {
			s.deps.Log.Warn("saved configuration values are not applied", "monitor", name, "paths", paths[i])
		}
	}
}
