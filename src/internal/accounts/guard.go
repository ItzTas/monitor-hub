package accounts

import (
	"context"

	"github.com/ItzTas/monitor-hub/src/internal/db"
)

// inGuardedTx refuses a change that leaves no active admin when there was one.
// The advisory lock stops two people from removing each other's admin at once.
func (s *Service) inGuardedTx(ctx context.Context, change func(q *db.Queries) error) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		if err := q.LockAccess(ctx); err != nil {
			return err
		}
		before, err := q.CountAdmins(ctx)
		if err != nil {
			return err
		}
		if err := change(q); err != nil {
			return err
		}
		after, err := q.CountAdmins(ctx)
		if err != nil {
			return err
		}
		if before > 0 && after == 0 {
			return ErrLastAdmin
		}
		return nil
	})
}
