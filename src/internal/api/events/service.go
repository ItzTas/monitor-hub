package events

import (
	"context"

	"github.com/ItzTas/monitor-hub/src/internal/access"
)

const maxLimit = 200

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// List gives only the events of the monitors p can view.
func (s *Service) List(ctx context.Context, p access.Principal, input ListInput) (Page, error) {
	names, err := s.repo.MonitorNames(ctx)
	if err != nil {
		return Page{}, err
	}
	limit := min(input.Limit, maxLimit)
	filter := Filter{Monitor: input.Monitor, Kind: input.Kind, Before: input.Before, Limit: limit + 1, Visible: access.Visible(p, names)}
	events, err := s.repo.ListEvents(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	if len(events) <= limit {
		return Page{Items: events}, nil
	}
	events = events[:limit]
	next := events[limit-1].Seq
	return Page{Items: events, NextBefore: &next}, nil
}
