package accounts

import "context"

// Prepare gives every catalog monitor a row, so permissions can name a monitor
// that has not described itself yet.
func (s *Service) Prepare(ctx context.Context, monitors []string) error {
	return s.queries.EnsureMonitors(ctx, monitors)
}

func (s *Service) MonitorNames(ctx context.Context) ([]string, error) {
	return s.queries.MonitorNames(ctx)
}
