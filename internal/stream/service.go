package stream

import (
	"context"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/ItzTas/monitor-hub/internal/icons"
)

const (
	PageSize     = 500
	PingInterval = 25 * time.Second
)

type Client struct {
	Name     string
	Position int64
	Check    func(context.Context) (bool, error)
	Clamp    func(ctx context.Context, latest int64) error
}

type Service struct {
	repo     Repository
	signal   *Signal
	registry *Registry
	after    func(time.Duration) <-chan time.Time
}

type Option func(*Service)

func WithClock(after func(time.Duration) <-chan time.Time) Option {
	return func(s *Service) { s.after = after }
}

func NewService(repo Repository, signal *Signal, registry *Registry, options ...Option) *Service {
	s := &Service{repo: repo, signal: signal, registry: registry, after: time.After}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Service) Stream(ctx context.Context, c Client, send sse.Sender) error {
	wake, unsubscribe := s.signal.Subscribe()
	defer unsubscribe()
	replaced, leave := s.registry.Join(c.Name)
	defer leave()

	latest, err := s.repo.Latest(ctx)
	if err != nil {
		return err
	}
	cursor := c.Position
	if cursor > latest {
		if err := c.Clamp(ctx, latest); err != nil {
			return err
		}
		cursor = latest
	}
	cursor, backlog, err := s.drain(ctx, cursor, send)
	if err != nil || s.stopped(ctx) {
		return err
	}
	seq := max(latest, cursor)
	caughtUp := sse.Message{IDString: strconv.FormatInt(seq, 10), Data: CaughtUp{Seq: seq, Backlog: backlog}}
	if err := send(caughtUp); err != nil {
		return err
	}

	ping := s.after(PingInterval)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.signal.Done():
			return nil
		case <-replaced:
			return send(sse.Message{Data: Replaced{}})
		case <-wake:
		case <-ping:
			ping = s.after(PingInterval)
			if err := send(sse.Message{Comment: "ping"}); err != nil {
				return err
			}
			holds, err := c.Check(ctx)
			if err != nil {
				return err
			}
			if !holds {
				return nil
			}
		}
		if cursor, _, err = s.drain(ctx, cursor, send); err != nil || s.stopped(ctx) {
			return err
		}
	}
}

func (s *Service) drain(ctx context.Context, cursor int64, send sse.Sender) (int64, int, error) {
	sent := 0
	for !s.stopped(ctx) {
		page, err := s.repo.After(ctx, cursor, PageSize)
		if err != nil {
			return cursor, sent, err
		}
		for _, n := range page {
			n.Icon = icons.Default()
			n.PluginIcon = icons.Default()
			if err := send(sse.Message{IDString: strconv.FormatInt(n.Seq, 10), Data: n}); err != nil {
				return cursor, sent, err
			}
			cursor = n.Seq
			sent++
		}
		if len(page) < PageSize {
			return cursor, sent, nil
		}
	}
	return cursor, sent, nil
}

func (s *Service) stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-s.signal.Done():
		return true
	default:
		return false
	}
}
