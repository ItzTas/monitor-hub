package stream

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/ingest"
)

func store(t *testing.T, pool *pgxpool.Pool, after int64, baseline bool, severity monitorpb.Severity, count int) {
	t.Helper()
	repo := ingest.NewPostgres(pool)
	if err := repo.SaveDescription(t.Context(), &monitorpb.DescribeResponse{Name: "sei", DisplayName: "SEI", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	var events []*monitorpb.Event
	for i := range count {
		seq := after + int64(i) + 1
		events = append(events, &monitorpb.Event{Seq: seq, Id: "e" + string(rune('a'+seq)), Kind: "ping", Severity: severity, OccurredAt: timestamppb.Now(), Title: "ping"})
	}
	if _, err := repo.StoreEvents(t.Context(), ingest.Batch{Plugin: "sei", After: after, Events: events, Baseline: baseline}); err != nil {
		t.Fatal(err)
	}
}

func seqs(list []Notification) []int64 {
	var out []int64
	for _, n := range list {
		out = append(out, n.Seq)
	}
	return out
}

func TestAfterSkipsBaselineEventsAndPages(t *testing.T) {
	pool := coretest.Pool(t)
	r := NewPostgres(pool)
	if got, err := r.Latest(t.Context()); err != nil || got != 0 {
		t.Fatalf("Latest on an empty database = %d, %v; want 0", got, err)
	}
	store(t, pool, 0, true, monitorpb.Severity_SEVERITY_INFO, 2)
	store(t, pool, 2, false, monitorpb.Severity_SEVERITY_INFO, 2)
	store(t, pool, 4, false, monitorpb.Severity_SEVERITY_URGENT, 1)
	for _, c := range []struct {
		after int64
		want  []int64
	}{{0, []int64{3, 4}}, {4, []int64{5}}, {5, nil}} {
		got, err := r.After(t.Context(), c.after, 2)
		if err != nil || !slices.Equal(seqs(got), c.want) {
			t.Fatalf("After(%d, 2) = %v, %v; want %v", c.after, seqs(got), err, c.want)
		}
	}
	got, err := r.After(t.Context(), 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Urgent || got[0].Monitor != "sei" || got[0].DisplayName != "SEI" {
		t.Fatalf("notification = %+v, want urgent from sei", got)
	}
	if latest, err := r.Latest(t.Context()); err != nil || latest != 5 {
		t.Fatalf("Latest = %d, %v; want 5", latest, err)
	}
}
