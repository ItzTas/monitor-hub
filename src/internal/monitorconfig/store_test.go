package monitorconfig

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

var savedAt = time.Date(2026, 11, 27, 10, 30, 0, 0, time.UTC)

func fullSaved() Saved {
	return Saved{
		Document:  map[string]json.RawMessage{"interval": json.RawMessage(`"10m"`), "mail": json.RawMessage(`{"host":"smtp.example.test","port":587}`)},
		Secrets:   map[string]string{"mail.password": "s3cret-value"},
		Ignored:   []IgnoredValue{{Path: "old.token", Value: json.RawMessage(`"hidden-value"`), Secret: true, Reason: ReasonReplaced}},
		Variables: map[string]Variable{"EXTRA": {Value: "1", Secret: false}},
		UpdatedAt: savedAt,
	}
}

func newStore(t *testing.T) *Postgres {
	t.Helper()
	return NewPostgres(coretest.Pool(t))
}

// same compares without printing the values: a stored document holds secrets.
func same(t *testing.T, got, want Saved) {
	t.Helper()
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("UpdatedAt = %v; want %v", got.UpdatedAt, want.UpdatedAt)
	}
	got.UpdatedAt, want.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(asJSON(t, got), asJSON(t, want)) {
		t.Error("the saved configuration differs from what was stored (values not shown)")
	}
}

// asJSON reads a Saved as plain JSON values, so key order and spacing do not count.
func asJSON(t *testing.T, saved Saved) any {
	t.Helper()
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var plain any
	if err := json.Unmarshal(data, &plain); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return plain
}

func empty() Saved {
	return Saved{Document: map[string]json.RawMessage{}, Secrets: map[string]string{}, Variables: map[string]Variable{}}
}

func TestGetWithoutARowIsEmptyAtRevisionZero(t *testing.T) {
	got, err := newStore(t).Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	same(t, got, empty())
	if got.Revision != 0 {
		t.Errorf("Revision = %d; want 0", got.Revision)
	}
}

func TestStoreKeepsEveryPart(t *testing.T) {
	store := newStore(t)
	want := fullSaved()
	revision, err := store.Put(t.Context(), "sei", want, 0)
	if err != nil || revision != 1 {
		t.Fatalf("Put = %d, %v; want 1", revision, err)
	}
	got, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want.Revision = 1
	same(t, got, want)

	next := fullSaved()
	next.Secrets = map[string]string{}
	revision, err = store.Put(t.Context(), "sei", next, 1)
	if err != nil || revision != 2 {
		t.Fatalf("second Put = %d, %v; want 2", revision, err)
	}
	got, err = store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	next.Revision = 2
	same(t, got, next)
}

func TestAStaleRevisionWritesNothing(t *testing.T) {
	store := newStore(t)
	other := fullSaved()
	if _, err := store.Put(t.Context(), "diario", other, 0); err != nil {
		t.Fatalf("Put diario: %v", err)
	}
	if _, err := store.Put(t.Context(), "sei", fullSaved(), 0); err != nil {
		t.Fatalf("Put sei: %v", err)
	}
	before, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	changed := empty()
	changed.UpdatedAt = savedAt.Add(time.Hour)
	for _, previous := range []int64{0, 2, 7} {
		if _, err := store.Put(t.Context(), "sei", changed, previous); !errors.Is(err, ErrStale) {
			t.Errorf("Put with previous %d: %v; want ErrStale", previous, err)
		}
	}
	after, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	same(t, after, before)

	if _, err := store.Put(t.Context(), "anhanguera", changed, 3); !errors.Is(err, ErrStale) {
		t.Errorf("Put without a row and previous 3: %v; want ErrStale", err)
	}
	created, err := store.Get(t.Context(), "anhanguera")
	if err != nil || created.Revision != 0 {
		t.Errorf("Get anhanguera = revision %d, %v; want 0 (no row)", created.Revision, err)
	}

	untouched, err := store.Get(t.Context(), "diario")
	if err != nil {
		t.Fatalf("Get diario: %v", err)
	}
	other.Revision = 1
	same(t, untouched, other)
}

func TestAValueWithNULIsRefusedByTheDatabase(t *testing.T) {
	store := newStore(t)
	if _, err := store.Put(t.Context(), "sei", fullSaved(), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	before, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	bad := fullSaved()
	bad.Secrets = map[string]string{"mail.password": "zq-before\x00zq-after"}
	_, err = store.Put(t.Context(), "sei", bad, 1)
	if err == nil {
		t.Fatal("Put with a NUL in a secret succeeded")
	}
	if errors.Is(err, ErrStale) {
		t.Errorf("Put = %v; want a database error, not ErrStale", err)
	}
	if strings.Contains(err.Error(), "zq-") {
		t.Error("the error repeats a stored value (not shown)")
	}
	after, err := store.Get(t.Context(), "sei")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	same(t, after, before)
}
