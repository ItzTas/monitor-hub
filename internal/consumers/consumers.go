package consumers

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/internal/db"
)

var (
	ErrNotFound      = errors.New("consumers: no such consumer")
	ErrExists        = errors.New("consumers: consumer already exists")
	ErrInvalidName   = errors.New("consumers: invalid consumer name")
	ErrSeqOutOfRange = errors.New("consumers: seq is outside the range of stored events")
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

func ValidName(name string) bool {
	return namePattern.MatchString(name) && !strings.HasSuffix(name, "-") && !strings.Contains(name, "--")
}

type Consumer struct {
	Name      string
	Position  int64
	Behind    int64
	CreatedAt time.Time
	AckedAt   *time.Time
	SeenAt    *time.Time
}

type Service struct {
	queries *db.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{queries: db.New(pool)}
}

func (s *Service) Add(ctx context.Context, name string) (string, error) {
	if !ValidName(name) {
		return "", ErrInvalidName
	}
	token, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.queries.InsertConsumer(ctx, db.InsertConsumerParams{Name: name, TokenHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrExists
	}
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Service) List(ctx context.Context) ([]Consumer, error) {
	rows, err := s.queries.ListConsumers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Consumer, 0, len(rows))
	for _, r := range rows {
		out = append(out, Consumer{Name: r.Name, Position: r.Position, Behind: r.Behind, CreatedAt: r.CreatedAt, AckedAt: r.AckedAt, SeenAt: r.SeenAt})
	}
	return out, nil
}

func (s *Service) Rotate(ctx context.Context, name string) (string, error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.queries.RotateConsumer(ctx, db.RotateConsumerParams{Name: name, TokenHash: hash})
	if err != nil {
		return "", notFound(err)
	}
	return token, nil
}

func (s *Service) Revoke(ctx context.Context, name string) error {
	_, err := s.queries.DeleteConsumer(ctx, name)
	return notFound(err)
}

func (s *Service) Authenticate(ctx context.Context, token string) (Consumer, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return Consumer{}, ErrNotFound
	}
	r, err := s.queries.ConsumerByTokenHash(ctx, HashToken(token))
	if err != nil {
		return Consumer{}, notFound(err)
	}
	return Consumer{Name: r.Name, Position: r.Position, Behind: r.Behind, CreatedAt: r.CreatedAt, AckedAt: r.AckedAt, SeenAt: r.SeenAt}, nil
}

func (s *Service) Touch(ctx context.Context, name, tokenHash string) (bool, error) {
	_, err := s.queries.TouchConsumer(ctx, db.TouchConsumerParams{Name: name, TokenHash: tokenHash})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) Ack(ctx context.Context, name string, seq int64) (int64, error) {
	latest, err := s.queries.LatestEventSeq(ctx)
	if err != nil {
		return 0, err
	}
	if seq < 0 || seq > latest {
		return 0, ErrSeqOutOfRange
	}
	position, err := s.queries.AckConsumer(ctx, db.AckConsumerParams{Name: name, Seq: seq})
	if err != nil {
		return 0, notFound(err)
	}
	return position, nil
}

func (s *Service) ClampPosition(ctx context.Context, name string, latest int64) error {
	return s.queries.ClampConsumerPosition(ctx, db.ClampConsumerPositionParams{Name: name, Latest: latest})
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
