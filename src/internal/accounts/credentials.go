package accounts

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

const credentialPrefix = "mhk_"

type Credential struct {
	Token
	Username string
}

var (
	ErrCredentialCarries = errors.New("a credential carries only hook of monitors")
	ErrUsernameTaken     = errors.New("a credential already uses this username")
	ErrInvalidUsername   = errors.New("usernames use lowercase letters, digits and single hyphens, 2 to 31 characters, starting with a letter")
	ErrUnknownCredential = errors.New("no credential has this id")
)

// unknownUsernameHash gives an unknown username the same comparison as a known one.
const unknownUsernameHash = "0000000000000000000000000000000000000000000000000000000000000000"

func (s *Service) CreateCredential(ctx context.Context, owner access.Principal, username string, req TokenRequest) (Credential, string, error) {
	if err := checkLabel(req.Label); err != nil {
		return Credential{}, "", err
	}
	if !consumers.ValidName(username) {
		return Credential{}, "", ErrInvalidUsername
	}
	if err := s.checkRequest(owner, req, []access.Action{access.Hook}, ErrCredentialCarries); err != nil {
		return Credential{}, "", err
	}
	secret, hash, err := NewSecret(credentialPrefix)
	if err != nil {
		return Credential{}, "", err
	}
	credential := Credential{Token: newToken(owner, req, s.now()), Username: username}
	err = s.inTx(ctx, func(q *db.Queries) error {
		id, err := q.InsertCredential(ctx, db.InsertCredentialParams{
			UserID: owner.UserID(), Label: req.Label, Username: username, SecretHash: hash,
			CreatedAt: credential.CreatedAt, ExpiresAt: req.ExpiresAt,
		})
		if err != nil {
			return usernameTaken(err)
		}
		credential.ID = id
		for _, p := range req.Permissions {
			err := q.InsertCredentialGrant(ctx, db.InsertCredentialGrantParams{CredentialID: id, Monitor: grantMonitor(p), Action: string(p.Action)})
			if err != nil {
				return unknownMonitor(err)
			}
		}
		return nil
	})
	if err != nil {
		return Credential{}, "", err
	}
	return credential, secret, nil
}

func usernameTaken(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "credentials_username_key" {
		return ErrUsernameTaken
	}
	return err
}

// Credentials lists the credentials of userID, or of everyone when userID is 0.
func (s *Service) Credentials(ctx context.Context, userID int64) ([]Credential, error) {
	rows, err := s.queries.ListCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	grants, err := s.queries.CredentialGrants(ctx, ids)
	if err != nil {
		return nil, err
	}
	permissions := make(map[int64][]access.Permission, len(rows))
	for _, g := range grants {
		permissions[g.CredentialID] = append(permissions[g.CredentialID], permission(g.Monitor, g.Action))
	}
	credentials := make([]Credential, 0, len(rows))
	for _, r := range rows {
		credentials = append(credentials, Credential{
			Token: Token{
				ID:          r.ID,
				Owner:       r.Login,
				Label:       r.Label,
				Permissions: permissions[r.ID],
				CreatedAt:   r.CreatedAt,
				ExpiresAt:   r.ExpiresAt,
				LastUsedAt:  r.LastUsedAt,
			},
			Username: r.Username,
		})
	}
	return credentials, nil
}

// RevokeCredential deletes the credential id of userID, or of anyone when userID is 0.
func (s *Service) RevokeCredential(ctx context.Context, id, userID int64) error {
	deleted, err := s.queries.DeleteCredential(ctx, db.DeleteCredentialParams{ID: id, UserID: userID})
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrUnknownCredential
	}
	return nil
}

func (s *Service) ByCredential(ctx context.Context, username, password string) (access.Principal, error) {
	now := s.now()
	row, err := s.queries.CredentialPerson(ctx, db.CredentialPersonParams{Username: username, Now: now})
	hash := []byte(HashSecret(password))
	if errors.Is(err, pgx.ErrNoRows) {
		subtle.ConstantTimeCompare(hash, []byte(unknownUsernameHash))
		return access.Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return access.Principal{}, err
	}
	if subtle.ConstantTimeCompare(hash, []byte(row.SecretHash)) != 1 {
		return access.Principal{}, ErrUnauthenticated
	}
	var (
		person access.Person
		grants []db.CredentialGrant
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { person, err = s.person(gctx, row.UserID, row.Login, row.Admin); return err })
	g.Go(func() (err error) { grants, err = s.queries.CredentialGrants(gctx, []int64{row.ID}); return err })
	g.Go(func() error { return s.queries.TouchCredential(gctx, db.TouchCredentialParams{ID: row.ID, Now: now}) })
	if err := g.Wait(); err != nil {
		return access.Principal{}, err
	}
	carried := make([]access.Permission, 0, len(grants))
	for _, grant := range grants {
		carried = append(carried, permission(grant.Monitor, grant.Action))
	}
	return access.NewCredential(person, carried), nil
}
