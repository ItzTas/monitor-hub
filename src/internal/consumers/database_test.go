package consumers

import (
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ItzTas/monitor-hub/sdk-go/pluginpb"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/ingest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMain(m *testing.M) {
	os.Exit(coretest.Run(m))
}

func addEvents(t *testing.T, pool *pgxpool.Pool, after int64, baseline bool, count int) {
	t.Helper()
	repo := ingest.NewPostgres(pool)
	if err := repo.SaveDescription(t.Context(), &pluginpb.DescribeResponse{Name: "sei", DisplayName: "sei", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	var events []*pluginpb.Event
	for i := range count {
		seq := after + int64(i) + 1
		events = append(events, &pluginpb.Event{Seq: seq, Id: "e" + string(rune('a'+seq)), Kind: "ping", OccurredAt: timestamppb.Now(), Title: "ping"})
	}
	if _, err := repo.StoreEvents(t.Context(), ingest.Batch{Plugin: "sei", After: after, Events: events, Baseline: baseline}); err != nil {
		t.Fatal(err)
	}
}

func only(t *testing.T, s *Service) Consumer {
	t.Helper()
	list, err := s.List(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v; want one consumer", list, err)
	}
	return list[0]
}

func TestANewConsumerStartsAtTheNewestEvent(t *testing.T) {
	pool := coretest.Pool(t)
	s := NewService(pool)
	addEvents(t, pool, 0, false, 4)
	if _, err := s.Add(t.Context(), "notebook"); err != nil {
		t.Fatal(err)
	}
	if c := only(t, s); c.Position != 4 || c.Behind != 0 {
		t.Fatalf("consumer = %+v, want position 4 and behind 0", c)
	}
	addEvents(t, pool, 4, false, 1)
	if c := only(t, s); c.Behind != 1 {
		t.Fatalf("behind = %d after a normal event, want 1", c.Behind)
	}
	addEvents(t, pool, 5, true, 1)
	if c := only(t, s); c.Behind != 1 {
		t.Fatalf("behind = %d after a baseline event, want 1", c.Behind)
	}
}

func TestAddRefusesDuplicatesAndBadNames(t *testing.T) {
	s := NewService(coretest.Pool(t))
	if _, err := s.Add(t.Context(), "notebook"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(t.Context(), "notebook"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate Add error = %v, want ErrExists", err)
	}
	if _, err := s.Add(t.Context(), "Bad Name"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("bad name Add error = %v, want ErrInvalidName", err)
	}
}

func TestAuthenticateFindsTheConsumerByToken(t *testing.T) {
	s := NewService(coretest.Pool(t))
	token, err := s.Add(t.Context(), "notebook")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Authenticate(t.Context(), token)
	if err != nil || c.Name != "notebook" {
		t.Fatalf("Authenticate = %+v, %v; want notebook", c, err)
	}
	other, _, _ := NewToken()
	for _, bad := range []string{other, HashToken(token), ""} {
		if _, err := s.Authenticate(t.Context(), bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Authenticate(%q) error = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestRotateKeepsThePositionAndRetiresTheOldToken(t *testing.T) {
	pool := coretest.Pool(t)
	s := NewService(pool)
	addEvents(t, pool, 0, false, 2)
	old, err := s.Add(t.Context(), "notebook")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Rotate(t.Context(), "notebook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), old); !errors.Is(err, ErrNotFound) {
		t.Errorf("old token error = %v, want ErrNotFound", err)
	}
	c, err := s.Authenticate(t.Context(), fresh)
	if err != nil || c.Position != 2 {
		t.Errorf("new token gave %+v, %v; want position 2", c, err)
	}
	if ok, err := s.Touch(t.Context(), "notebook", HashToken(old)); err != nil || ok {
		t.Errorf("Touch with the old hash = %v, %v; want false", ok, err)
	}
	if ok, err := s.Touch(t.Context(), "notebook", HashToken(fresh)); err != nil || !ok {
		t.Errorf("Touch with the new hash = %v, %v; want true", ok, err)
	}
	if _, err := s.Rotate(t.Context(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Rotate of a missing consumer error = %v, want ErrNotFound", err)
	}
}

func TestRevokeRemovesTheConsumer(t *testing.T) {
	s := NewService(coretest.Pool(t))
	token, err := s.Add(t.Context(), "notebook")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), "notebook"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), token); !errors.Is(err, ErrNotFound) {
		t.Errorf("Authenticate error = %v, want ErrNotFound", err)
	}
	if err := s.Revoke(t.Context(), "notebook"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Revoke error = %v, want ErrNotFound", err)
	}
}

func TestAckOnlyMovesForwardAndStopsAtTheNewestEvent(t *testing.T) {
	pool := coretest.Pool(t)
	s := NewService(pool)
	addEvents(t, pool, 0, false, 3)
	token, err := s.Add(t.Context(), "notebook")
	if err != nil {
		t.Fatal(err)
	}
	addEvents(t, pool, 3, false, 3)
	for _, step := range []struct {
		seq, want int64
		err       error
	}{
		{5, 5, nil},
		{4, 5, nil},
		{7, 0, ErrSeqOutOfRange},
		{-1, 0, ErrSeqOutOfRange},
	} {
		got, err := s.Ack(t.Context(), "notebook", HashToken(token), step.seq)
		if !errors.Is(err, step.err) || got != step.want {
			t.Errorf("Ack(%d) = %d, %v; want %d, %v", step.seq, got, err, step.want, step.err)
		}
	}
	c := only(t, s)
	if c.AckedAt == nil || c.SeenAt == nil {
		t.Errorf("acked_at and seen_at = %v, %v; want both set", c.AckedAt, c.SeenAt)
	}
	if _, err := s.Ack(t.Context(), "ghost", HashToken(token), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Ack of a missing consumer error = %v, want ErrNotFound", err)
	}
}

func TestAckCountsOnlyWithTheConsumersCurrentToken(t *testing.T) {
	pool := coretest.Pool(t)
	s := NewService(pool)
	addEvents(t, pool, 0, false, 1)
	old, err := s.Add(t.Context(), "notebook")
	if err != nil {
		t.Fatal(err)
	}
	addEvents(t, pool, 1, false, 2)
	rotated, err := s.Rotate(t.Context(), "notebook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ack(t.Context(), "notebook", HashToken(old), 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("Ack with the token from before the rotation error = %v, want ErrNotFound", err)
	}
	if err := s.Revoke(t.Context(), "notebook"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(t.Context(), "notebook"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ack(t.Context(), "notebook", HashToken(rotated), 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("Ack with the token of the revoked consumer error = %v, want ErrNotFound", err)
	}
	if c := only(t, s); c.Position != 3 || c.AckedAt != nil {
		t.Errorf("consumer = %+v, want position 3 and never acknowledged", c)
	}
}

func TestClampPositionLowersAPositionAboveTheSeqAndNeverRaisesOne(t *testing.T) {
	pool := coretest.Pool(t)
	s := NewService(pool)
	addEvents(t, pool, 0, false, 4)
	if _, err := s.Add(t.Context(), "notebook"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClampPosition(t.Context(), "notebook", 9); err != nil {
		t.Fatal(err)
	}
	if c := only(t, s); c.Position != 4 {
		t.Fatalf("position = %d after clamping to 9, want it to stay 4", c.Position)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE consumers SET position = 90`); err != nil {
		t.Fatal(err)
	}
	if err := s.ClampPosition(t.Context(), "notebook", 4); err != nil {
		t.Fatal(err)
	}
	if c := only(t, s); c.Position != 4 {
		t.Fatalf("position = %d after clamping 90 to 4, want 4", c.Position)
	}
}
