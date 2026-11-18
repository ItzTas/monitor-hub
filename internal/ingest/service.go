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
	wake        func()
	after       func(time.Duration) <-chan time.Time
	schedules   map[string]*schedule
	onPolled    func(name string, req pollRequest)
}

type Option func(*Service)

func WithWake(wake func()) Option {
	return func(s *Service) { s.wake = wake }
}

func NewService(repo Repository, clients Clients, channels []string, log *slog.Logger, now func() time.Time, options ...Option) *Service {
	s := &Service{
		repo:        repo,
		clients:     clients,
		channels:    channels,
		log:         log,
		now:         now,
		callTimeout: 30 * time.Second,
		wake:        func() {},
		after:       time.After,
		schedules:   map[string]*schedule{},
	}
	for _, name := range clients.Names() {
		s.schedules[name] = newSchedule()
	}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for name, sch := range s.schedules {
		wg.Go(func() { s.loop(ctx, name, sch) })
		wg.Go(func() { s.subscribe(ctx, name, sch) })
	}
	wg.Wait()
	return nil
}

func (s *Service) loop(ctx context.Context, name string, sch *schedule) {
	var timer <-chan time.Time
	for {
		if timer == nil {
			timer = s.after(sch.interval())
		}
		if !s.wait(ctx, sch, timer) {
			return
		}
		req, ok := sch.take()
		if !ok {
			continue
		}
		s.pollPlugin(ctx, name, req)
		if req.full {
			timer = nil
		}
		followUp := sch.pending()
		s.polled(name, req)
		if !followUp {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.after(followUpDelay):
		}
	}
}

func (s *Service) wait(ctx context.Context, sch *schedule, timer <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-sch.kick:
	case <-timer:
		sch.request(pollRequest{full: true})
	}
	return ctx.Err() == nil
}

func (s *Service) PollOnce(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range s.clients.Names() {
		wg.Go(func() { s.pollPlugin(ctx, name, pollRequest{full: true}) })
	}
	wg.Wait()
}

func (s *Service) pollPlugin(ctx context.Context, name string, req pollRequest) {
	client, ok := s.clients.Client(name)
	if !ok {
		return
	}
	if err := s.poll(ctx, name, client, req); err != nil {
		s.log.Warn("poll a plugin", "plugin", name, "error", err)
	}
}

func (s *Service) polled(name string, req pollRequest) {
	if s.onPolled == nil {
		return
	}
	s.onPolled(name, req)
}

func (s *Service) poll(ctx context.Context, name string, client pluginpb.PluginClient, req pollRequest) error {
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
	if !req.readsEvents(cursor, known) {
		s.refreshStatus(ctx, name, client)
		return nil
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
		if inserted > 0 && !baseline && len(s.channels) > 0 {
			s.wake()
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
