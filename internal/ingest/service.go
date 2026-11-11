package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

const (
	PollInterval   = 15 * time.Second
	EventsPageSize = 500
)

type Service struct {
	repo        Repository
	clients     Clients
	channels    []string
	log         *slog.Logger
	now         func() time.Time
	callTimeout time.Duration
}

func NewService(repo Repository, clients Clients, channels []string, log *slog.Logger, now func() time.Time) *Service {
	return &Service{
		repo:        repo,
		clients:     clients,
		channels:    channels,
		log:         log,
		now:         now,
		callTimeout: 30 * time.Second,
	}
}

func (s *Service) Run(ctx context.Context) error {
	for {
		s.PollOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(PollInterval):
		}
	}
}

func (s *Service) PollOnce(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range s.clients.Names() {
		client, ok := s.clients.Client(name)
		if !ok {
			continue
		}
		wg.Go(func() {
			if err := s.poll(ctx, name, client); err != nil {
				s.log.Warn("poll a plugin", "plugin", name, "error", err)
			}
		})
	}
	wg.Wait()
}

func (s *Service) poll(ctx context.Context, name string, client pluginpb.PluginClient) error {
	var (
		cursor int64
		known  bool
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { cursor, known, err = s.repo.Cursor(gctx, name); return err })
	g.Go(func() error { return s.describe(gctx, name, client) })
	if err := g.Wait(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Go(func() { s.refreshStatus(ctx, name, client) })
	err := s.ingest(ctx, name, client, cursor, !known)
	wg.Wait()
	return err
}

func (s *Service) describe(ctx context.Context, name string, client pluginpb.PluginClient) error {
	description, err := call(ctx, s.callTimeout, client.Describe, &pluginpb.DescribeRequest{})
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}
	if description.GetName() != name {
		return fmt.Errorf("the plugin describes itself as %q", description.GetName())
	}
	if err := s.repo.SaveDescription(ctx, description); err != nil {
		return fmt.Errorf("save the description: %w", err)
	}
	return nil
}

func (s *Service) refreshStatus(ctx context.Context, name string, client pluginpb.PluginClient) {
	status, err := call(ctx, s.callTimeout, client.Status, &pluginpb.StatusRequest{})
	if err != nil {
		s.log.Warn("read a plugin status", "plugin", name, "error", err)
		return
	}
	if err := s.repo.SaveStatus(ctx, name, status, s.now()); err != nil {
		s.log.Error("save a plugin status", "plugin", name, "error", err)
	}
}

func (s *Service) ingest(ctx context.Context, name string, client pluginpb.PluginClient, cursor int64, baseline bool) error {
	for {
		page, err := call(ctx, s.callTimeout, client.Events, &pluginpb.EventsRequest{After: cursor, Limit: EventsPageSize})
		if err != nil {
			return fmt.Errorf("read events after %d: %w", cursor, err)
		}
		events := page.GetEvents()
		if len(events) > EventsPageSize {
			return fmt.Errorf("the page after %d has %d events, over the limit of %d", cursor, len(events), EventsPageSize)
		}
		inserted, err := s.repo.StoreEvents(ctx, Batch{
			Plugin:   name,
			After:    cursor,
			Events:   events,
			Channels: s.channels,
			Baseline: baseline,
			Now:      s.now(),
		})
		if err != nil {
			return fmt.Errorf("store events after %d: %w", cursor, err)
		}
		if inserted > 0 {
			s.log.Info("stored events", "plugin", name, "count", inserted, "baseline", baseline)
		}
		if len(events) < EventsPageSize {
			break
		}
		cursor = events[len(events)-1].GetSeq()
	}
	if !baseline {
		return nil
	}
	if err := s.repo.CompleteBaseline(ctx, name); err != nil {
		return fmt.Errorf("complete the baseline: %w", err)
	}
	s.log.Info("baseline complete", "plugin", name)
	return nil
}

func call[Req, Resp any](ctx context.Context, timeout time.Duration, rpc func(context.Context, Req, ...grpc.CallOption) (Resp, error), req Req) (Resp, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return rpc(ctx, req)
}
