package accounts

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

var ErrUnknownMonitor = errors.New("no monitor has this name")

const (
	foreignKeyViolation = "23503"
	grantMonitorKey     = "user_grants_monitor_fkey"
)

// SetPermissions replaces the direct grants of login. The caller checks Admin.
func (s *Service) SetPermissions(ctx context.Context, login string, perms []access.Permission) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		// Under the lock, two replacements of the same list cannot merge.
		if err := q.LockAccess(ctx); err != nil {
			return err
		}
		user, err := q.UserByLogin(ctx, login)
		if err != nil {
			return unknownUser(err)
		}
		if err := q.DeleteUserGrants(ctx, user.ID); err != nil {
			return err
		}
		for _, p := range perms {
			if err := q.InsertUserGrant(ctx, grantRow(user.ID, p)); err != nil {
				return unknownMonitor(err)
			}
		}
		return nil
	})
}

// grantRow writes Every as a NULL monitor.
func grantRow(userID int64, p access.Permission) db.InsertUserGrantParams {
	monitor := pgtype.Text{String: p.Monitor, Valid: p.Monitor != access.Every}
	return db.InsertUserGrantParams{UserID: userID, Monitor: monitor, Action: string(p.Action)}
}

func unknownMonitor(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == grantMonitorKey {
		return ErrUnknownMonitor
	}
	return err
}
