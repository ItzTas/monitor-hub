package health

import (
	"context"
	"time"
)

const pingTimeout = 3 * time.Second

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return s.repo.Ping(ctx)
}
