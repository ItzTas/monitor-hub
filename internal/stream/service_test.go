package stream

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/ItzTas/monitor-hub/internal/icons"
)

type fakeRepo struct {
	mu      sync.Mutex
	events  []Notification
	latest  int64
	limits  []int
	onAfter func(call int)
	calls   int
}

func (f *fakeRepo) add(seqs ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range seqs {
		f.events = append(f.events, Notification{Seq: s, Title: "t" + strconv.FormatInt(s, 10)})
	}
}

func (f *fakeRepo) After(_ context.Context, after int64, limit int) ([]Notification, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.limits = append(f.limits, limit)
	hook := f.onAfter
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Notification
	for _, n := range f.events {
		if n.Seq > after && len(out) < limit {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeRepo) Latest(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest, nil
}

type harness struct {
	t       *testing.T
	repo    *fakeRepo
	signal  *Signal
	reg     *Registry
	service *Service
	tick    chan time.Time
	mu      sync.Mutex
	waits   []time.Duration
}

func newHarness(t *testing.T, repo *fakeRepo) *harness {
	t.Helper()
	h := &harness{t: t, repo: repo, signal: NewSignal(), reg: NewRegistry(), tick: make(chan time.Time, 1)}
	h.service = NewService(repo, h.signal, h.reg, WithClock(func(d time.Duration) <-chan time.Time {
		h.mu.Lock()
		h.waits = append(h.waits, d)
		h.mu.Unlock()
		return h.tick
	}))
	return h
}

type run struct {
	msgs chan sse.Message
	done chan error
}

func (h *harness) start(c Client) *run {
	h.t.Helper()
	return h.startWith(context.Background(), c, nil)
}

func (h *harness) startWith(ctx context.Context, c Client, fail error) *run {
	h.t.Helper()
	r := &run{msgs: make(chan sse.Message, 4096), done: make(chan error, 1)}
	go func() {
		r.done <- h.service.Stream(ctx, c, func(m sse.Message) error {
			if fail != nil {
				return fail
			}
			r.msgs <- m
			return nil
		})
	}()
	return r
}

func (r *run) next(t *testing.T) sse.Message {
	t.Helper()
	select {
	case m := <-r.msgs:
		return m
	case err := <-r.done:
		select {
		case m := <-r.msgs:
			r.done <- err
			return m
		default:
		}
		t.Fatalf("stream ended while waiting for a message: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a message")
	}
	return sse.Message{}
}

func (r *run) end(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the stream to end")
	}
	return nil
}

func (r *run) notification(t *testing.T) Notification {
	t.Helper()
	m := r.next(t)
	n, ok := m.Data.(Notification)
	if !ok {
		t.Fatalf("expected a notification, got %#v", m)
	}
	if m.IDString != strconv.FormatInt(n.Seq, 10) {
		t.Fatalf("IDString = %q, want %d", m.IDString, n.Seq)
	}
	return n
}

func (r *run) caughtUp(t *testing.T) CaughtUp {
	t.Helper()
	m := r.next(t)
	c, ok := m.Data.(CaughtUp)
	if !ok {
		t.Fatalf("expected caught_up, got %#v", m)
	}
	if m.IDString != strconv.FormatInt(c.Seq, 10) {
		t.Fatalf("caught_up IDString = %q, want %d", m.IDString, c.Seq)
	}
	return c
}

func TestTheBacklogStartsAfterTheConsumersPosition(t *testing.T) {
	repo := &fakeRepo{latest: 5}
	repo.add(1, 2, 3, 4, 5)
	h := newHarness(t, repo)
	r := h.start(Client{Name: "desk", Position: 2})
	for _, want := range []int64{3, 4, 5} {
		n := r.notification(t)
		if n.Seq != want || n.Icon != icons.Default() || n.PluginIcon != icons.Default() {
			t.Fatalf("got %+v, want seq %d with the default icons", n, want)
		}
	}
	if got := r.caughtUp(t); got != (CaughtUp{Seq: 5, Backlog: 3}) {
		t.Fatalf("got %+v", got)
	}
}

func TestAConsumerAtTheEndGetsOnlyCaughtUp(t *testing.T) {
	repo := &fakeRepo{latest: 7}
	repo.add(7)
	h := newHarness(t, repo)
	r := h.start(Client{Name: "desk", Position: 7})
	if got := r.caughtUp(t); got != (CaughtUp{Seq: 7, Backlog: 0}) {
		t.Fatalf("got %+v", got)
	}
}

func TestTheBacklogIsReadInPages(t *testing.T) {
	repo := &fakeRepo{latest: 1201}
	for i := int64(1); i <= 1201; i++ {
		repo.add(i)
	}
	h := newHarness(t, repo)
	r := h.start(Client{Name: "desk"})
	for i := int64(1); i <= 1201; i++ {
		if n := r.notification(t); n.Seq != i {
			t.Fatalf("got seq %d, want %d", n.Seq, i)
		}
	}
	if got := r.caughtUp(t); got != (CaughtUp{Seq: 1201, Backlog: 1201}) {
		t.Fatalf("got %+v", got)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if !slices.Equal(repo.limits, []int{500, 500, 500}) {
		t.Fatalf("limits = %v", repo.limits)
	}
}

func TestAPositionAheadOfTheHubStartsFromTheEnd(t *testing.T) {
	repo := &fakeRepo{latest: 4}
	repo.add(1, 2, 3, 4)
	h := newHarness(t, repo)
	r := h.start(Client{Name: "desk", Position: 90})
	if got := r.caughtUp(t); got != (CaughtUp{Seq: 4, Backlog: 0}) {
		t.Fatalf("got %+v", got)
	}
	repo.add(5)
	h.signal.Notify()
	if n := r.notification(t); n.Seq != 5 {
		t.Fatalf("got seq %d", n.Seq)
	}
}

func TestAnEventStoredWhileTheBacklogIsReadIsSentOnce(t *testing.T) {
	repo := &fakeRepo{latest: 5}
	repo.add(5)
	h := newHarness(t, repo)
	repo.onAfter = func(call int) {
		if call == 1 {
			repo.add(6)
			h.signal.Notify()
		}
	}
	r := h.start(Client{Name: "desk", Position: 4, Check: func(context.Context) (bool, error) { return true, nil }})
	var got []int64
	for len(got) < 2 {
		m := r.next(t)
		if n, ok := m.Data.(Notification); ok {
			got = append(got, n.Seq)
		}
	}
	h.signal.Notify()
	h.signal.Notify()
	h.tick <- time.Now()
	for {
		m := r.next(t)
		if n, ok := m.Data.(Notification); ok {
			t.Fatalf("unexpected extra notification %d", n.Seq)
		}
		if m.Comment == "ping" {
			break
		}
	}
	if !slices.Equal(got, []int64{5, 6}) {
		t.Fatalf("got %v", got)
	}
}

func TestAPingIsSentEvery25SecondsAndChecksTheConsumer(t *testing.T) {
	repo := &fakeRepo{}
	h := newHarness(t, repo)
	checks := 0
	r := h.start(Client{Name: "desk", Check: func(context.Context) (bool, error) {
		checks++
		return true, nil
	}})
	r.caughtUp(t)
	h.tick <- time.Now()
	if m := r.next(t); m.Comment != "ping" {
		t.Fatalf("got %#v", m)
	}
	h.signal.Close()
	if err := r.end(t); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if checks != 1 || h.waits[0] != 25*time.Second {
		t.Fatalf("checks = %d, waits = %v", checks, h.waits)
	}
}

func TestTheStreamEndsWhenTheConsumerNoLongerHoldsItsToken(t *testing.T) {
	h := newHarness(t, &fakeRepo{})
	r := h.start(Client{Name: "desk", Check: func(context.Context) (bool, error) { return false, nil }})
	r.caughtUp(t)
	h.tick <- time.Now()
	if err := r.end(t); err != nil {
		t.Fatal(err)
	}
}

func TestACheckFailureEndsTheStreamWithTheError(t *testing.T) {
	h := newHarness(t, &fakeRepo{})
	boom := errors.New("boom")
	r := h.start(Client{Name: "desk", Check: func(context.Context) (bool, error) { return false, boom }})
	r.caughtUp(t)
	h.tick <- time.Now()
	if err := r.end(t); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestANewerConnectionReplacesTheOlder(t *testing.T) {
	h := newHarness(t, &fakeRepo{})
	first := h.start(Client{Name: "desk"})
	first.caughtUp(t)
	second := h.start(Client{Name: "desk"})
	second.caughtUp(t)
	if m := first.next(t); m.Data != (Replaced{}) {
		t.Fatalf("got %#v", m)
	}
	if err := first.end(t); err != nil {
		t.Fatal(err)
	}
	if !h.reg.Connected("desk") {
		t.Fatal("the newer connection should still be registered")
	}
	h.signal.Close()
	if err := second.end(t); err != nil {
		t.Fatal(err)
	}
}

func TestTheStreamEndsWhenTheSignalCloses(t *testing.T) {
	h := newHarness(t, &fakeRepo{})
	r := h.start(Client{Name: "desk"})
	r.caughtUp(t)
	h.signal.Close()
	if err := r.end(t); err != nil {
		t.Fatal(err)
	}
	if h.reg.Connected("desk") {
		t.Fatal("the connection should be unregistered")
	}
}

func TestTheStreamEndsWhenTheContextEnds(t *testing.T) {
	h := newHarness(t, &fakeRepo{})
	ctx, cancel := context.WithCancel(context.Background())
	r := h.startWith(ctx, Client{Name: "desk"}, nil)
	r.caughtUp(t)
	cancel()
	if err := r.end(t); err != nil {
		t.Fatal(err)
	}
}

func TestAFailedSendEndsTheStream(t *testing.T) {
	boom := errors.New("closed")
	repo := &fakeRepo{latest: 1}
	repo.add(1)
	h := newHarness(t, repo)
	r := h.startWith(context.Background(), Client{Name: "desk"}, boom)
	if err := r.end(t); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}
