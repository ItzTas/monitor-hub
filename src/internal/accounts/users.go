package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

type User struct {
	Login       string
	Admin       bool
	Disabled    bool
	CreatedAt   time.Time
	Permissions []access.Permission
}

type UserChange struct {
	Admin    *bool // nil leaves it as it is
	Disabled *bool
}

func (s *Service) CreateUser(ctx context.Context, login, password string, admin bool) error {
	if !consumers.ValidName(login) {
		return ErrInvalidLogin
	}
	if err := checkPassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.queries.InsertUser(ctx, db.InsertUserParams{Login: login, PasswordHash: hash, Admin: admin, CreatedAt: s.now()})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUserExists
	}
	return err
}

func (s *Service) SetPassword(ctx context.Context, login, password string) error {
	if err := checkPassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(q *db.Queries) error {
		id, err := q.SetUserPassword(ctx, db.SetUserPasswordParams{Login: login, PasswordHash: hash})
		if err != nil {
			return unknownUser(err)
		}
		return q.DeleteUserSessions(ctx, id)
	})
}

func (s *Service) UpdateUser(ctx context.Context, login string, change UserChange) error {
	return s.inGuardedTx(ctx, func(q *db.Queries) error {
		user, err := q.UserByLogin(ctx, login)
		if err != nil {
			return unknownUser(err)
		}
		if change.Admin != nil {
			if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: user.ID, Admin: *change.Admin}); err != nil {
				return err
			}
		}
		if change.Disabled == nil {
			return nil
		}
		return q.SetUserDisabled(ctx, db.SetUserDisabledParams{ID: user.ID, Disabled: *change.Disabled, Now: s.now()})
	})
}

func (s *Service) DeleteUser(ctx context.Context, login string) error {
	return s.inGuardedTx(ctx, func(q *db.Queries) error {
		deleted, err := q.DeleteUser(ctx, login)
		if err != nil {
			return err
		}
		if deleted == 0 {
			return ErrUnknownUser
		}
		return nil
	})
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.queries.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	grants, err := s.queries.UserGrants(ctx, ids)
	if err != nil {
		return nil, err
	}
	permissions := make(map[int64][]access.Permission, len(rows))
	for _, g := range grants {
		permissions[g.UserID] = append(permissions[g.UserID], permission(g))
	}
	users := make([]User, 0, len(rows))
	for _, r := range rows {
		users = append(users, User{
			Login:       r.Login,
			Admin:       r.Admin,
			Disabled:    r.DisabledAt != nil,
			CreatedAt:   r.CreatedAt,
			Permissions: permissions[r.ID],
		})
	}
	return users, nil
}

// permission reads a grant row, where a NULL monitor means every monitor.
func permission(g db.UserGrant) access.Permission {
	monitor := access.Every
	if g.Monitor.Valid {
		monitor = g.Monitor.String
	}
	return access.Permission{Monitor: monitor, Action: access.Action(g.Action)}
}
