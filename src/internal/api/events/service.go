package events

import "context"

const maxLimit = 200

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) List(ctx context.Context, input ListInput) (Page, error) {
	limit := min(input.Limit, maxLimit)
	events, err := s.repo.ListEvents(ctx, Filter{Plugin: input.Plugin, Kind: input.Kind, Before: input.Before, Limit: limit + 1})
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
