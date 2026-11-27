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
	uniqueViolation     = "23505"
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
			if err := q.InsertUserGrant(ctx, db.InsertUserGrantParams{UserID: user.ID, Monitor: grantMonitor(p), Action: string(p.Action)}); err != nil {
				return unknownMonitor(err)
			}
		}
		return nil
	})
}

// grantMonitor writes Every as a NULL monitor.
func grantMonitor(p access.Permission) pgtype.Text {
	return pgtype.Text{String: p.Monitor, Valid: p.Monitor != access.Every}
}

func unknownMonitor(err error) error {
	var pgErr *pgconn.PgError
	monitorKeys := map[string]bool{
		"user_grants_monitor_fkey":       true,
		"token_grants_monitor_fkey":      true,
		"credential_grants_monitor_fkey": true,
	}
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && monitorKeys[pgErr.ConstraintName] {
		return ErrUnknownMonitor
	}
	return err
}
