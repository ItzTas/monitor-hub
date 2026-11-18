package consumerapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/src/internal/ingest"
	"github.com/ItzTas/monitor-hub/src/internal/stream"
)

const (
	apiToken = "api-token-0123456789abcdef"
	wait     = 5 * time.Second
)

type hub struct {
	pool      *pgxpool.Pool
	consumers *consumers.Service
	signal    *stream.Signal
	tick      chan time.Time
	server    *httptest.Server
}

func newHub(t *testing.T) *hub {
	t.Helper()
	pool := coretest.Pool(t)
	h := &hub{pool: pool, consumers: consumers.NewService(pool), signal: stream.NewSignal(), tick: make(chan time.Time)}
	streams := stream.NewService(stream.NewPostgres(pool), h.signal, stream.NewRegistry(), stream.WithClock(func(time.Duration) <-chan time.Time { return h.tick }))
	mux := http.NewServeMux()
	Register(mux, Deps{Consumers: h.consumers, Stream: streams, Log: slog.New(slog.DiscardHandler)})
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)
	t.Cleanup(h.signal.Close)
	return h
}

func (h *hub) store(t *testing.T, from, to int64) {
	t.Helper()
	repo := ingest.NewPostgres(h.pool)
	if err := repo.SaveDescription(t.Context(), &monitorpb.DescribeResponse{Name: "sei", DisplayName: "SEI", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	var events []*monitorpb.Event
	for seq := from; seq <= to; seq++ {
		events = append(events, &monitorpb.Event{Seq: seq, Id: fmt.Sprintf("e%d", seq), Kind: "ping", OccurredAt: timestamppb.Now(), Title: fmt.Sprintf("ping %d", seq)})
	}
	if _, err := repo.StoreEvents(t.Context(), ingest.Batch{Plugin: "sei", After: from - 1, Events: events}); err != nil {
		t.Fatal(err)
	}
}

func (h *hub) add(t *testing.T, name string) string {
	t.Helper()
	token, err := h.consumers.Add(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

const streamPath = "/consumer/v1/stream"

type reply struct {
	status int
	header http.Header
	body   string
}

func (h *hub) send(t *testing.T, method, path, body string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range header {
		if value != "" {
			req.Header.Set(name, value)
		}
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *hub) call(t *testing.T, method, path, body string, header map[string]string) reply {
	t.Helper()
	resp := h.send(t, method, path, body, header)
	defer func() { _ = resp.Body.Close() }()
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(content)}
}

func (h *hub) accepts(t *testing.T, token string) bool {
	t.Helper()
	resp := h.send(t, http.MethodGet, streamPath, "", map[string]string{"Authorization": "Bearer " + token})
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (h *hub) open(t *testing.T, token, lastEventID string) *events {
	t.Helper()
	resp := h.send(t, http.MethodGet, streamPath, "", map[string]string{"Authorization": "Bearer " + token, "Last-Event-ID": lastEventID})
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream = %d %s, want 200", resp.StatusCode, body)
	}
	return listen(t, resp)
}

func (h *hub) ack(t *testing.T, token, body string) reply {
	t.Helper()
	return h.call(t, http.MethodPost, "/consumer/v1/ack", body, map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
}

func (h *hub) ping(t *testing.T) {
	t.Helper()
	select {
	case h.tick <- time.Now():
	case <-time.After(wait):
		t.Fatalf("no stream waited for the ping within %s", wait)
	}
}

type frame struct {
	id, event, data, comment string
}

func (f *frame) set(line string) {
	field, value, _ := strings.Cut(line, ": ")
	targets := map[string]*string{"id": &f.id, "event": &f.event, "data": &f.data, "": &f.comment}
	if target, ok := targets[field]; ok {
		*target = value
	}
}

type events struct {
	body   io.Closer
	frames chan frame
	stop   chan struct{}
}

func listen(t *testing.T, resp *http.Response) *events {
	t.Helper()
	e := &events{body: resp.Body, frames: make(chan frame), stop: make(chan struct{})}
	go e.read(resp.Body)
	t.Cleanup(func() { close(e.stop) })
	return e
}

func (e *events) read(body io.Reader) {
	defer close(e.frames)
	scanner := bufio.NewScanner(body)
	current := frame{}
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			current.set(line)
			continue
		}
		select {
		case e.frames <- current:
		case <-e.stop:
			return
		}
		current = frame{}
	}
}

func (e *events) next(t *testing.T) frame {
	t.Helper()
	select {
	case f, ok := <-e.frames:
		if !ok {
			t.Fatal("the stream ended while a message was expected")
		}
		return f
	case <-time.After(wait):
		t.Fatalf("no message within %s", wait)
	}
	return frame{}
}

func (e *events) untilCaughtUp(t *testing.T) []frame {
	t.Helper()
	var got []frame
	for {
		f := e.next(t)
		got = append(got, f)
		if f.event == "caught_up" {
			return got
		}
	}
}

func (e *events) ended(t *testing.T) {
	t.Helper()
	select {
	case f, ok := <-e.frames:
		if ok {
			t.Fatalf("got %+v, want the stream to end", f)
		}
	case <-time.After(wait):
		t.Fatalf("the stream did not end within %s", wait)
	}
}

func (e *events) close() {
	_ = e.body.Close()
}

func title(t *testing.T, f frame) string {
	t.Helper()
	var n stream.Notification
	if err := json.Unmarshal([]byte(f.data), &n); err != nil {
		t.Fatalf("notification data %q: %v", f.data, err)
	}
	return n.Title
}

func TestTheContractRequiresAConsumerToken(t *testing.T) {
	h := newHub(t)
	token := h.add(t, "desk")
	unknown, _, err := consumers.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{
		"no header":     "",
		"unknown token": "Bearer " + unknown,
		"the API token": "Bearer " + apiToken,
		"basic":         "Basic " + token,
		"no scheme":     token,
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, streamPath, ""},
		{http.MethodPost, "/consumer/v1/ack", `{"seq":0}`},
	} {
		for name, header := range headers {
			got := h.call(t, route.method, route.path, route.body, map[string]string{"Authorization": header, "Content-Type": "application/json"})
			if got.status != http.StatusUnauthorized || got.header.Get("WWW-Authenticate") != "Bearer" || !strings.Contains(got.body, "missing or invalid consumer token") {
				t.Errorf("%s %s with %s = %d %s, want 401 with WWW-Authenticate: Bearer", route.method, route.path, name, got.status, got.body)
			}
		}
	}
}

func TestTheBearerSchemeIsCaseInsensitive(t *testing.T) {
	h := newHub(t)
	token := h.add(t, "desk")
	for _, scheme := range []string{"bearer", "BEARER"} {
		header := map[string]string{"Authorization": scheme + " " + token, "Content-Type": "application/json"}
		resp := h.send(t, http.MethodGet, streamPath, "", header)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("stream with %q = %d, want 200", scheme, resp.StatusCode)
		}
		if got := h.call(t, http.MethodPost, "/consumer/v1/ack", `{"seq":0}`, header); got.status != http.StatusOK {
			t.Errorf("ack with %q = %d %s, want 200", scheme, got.status, got.body)
		}
	}
}

func TestTheStreamStartsAtTheConsumersPosition(t *testing.T) {
	h := newHub(t)
	h.store(t, 1, 2)
	token := h.add(t, "desk")
	h.store(t, 3, 4)

	for _, lastEventID := range []string{"", "4"} {
		s := h.open(t, token, lastEventID)
		got := s.untilCaughtUp(t)
		s.close()
		if len(got) != 3 {
			t.Fatalf("Last-Event-ID %q: got %+v, want two notifications and caught_up", lastEventID, got)
		}
		for i, want := range []string{"3", "4"} {
			if got[i].id != want || got[i].event != "notification" || title(t, got[i]) != "ping "+want {
				t.Errorf("Last-Event-ID %q: message %d = %+v, want notification %s", lastEventID, i, got[i], want)
			}
		}
		if want := (frame{id: "4", event: "caught_up", data: `{"seq":4,"backlog":2}`}); got[2] != want {
			t.Errorf("Last-Event-ID %q: got %+v, want %+v", lastEventID, got[2], want)
		}
	}
}

func TestAckAnswersThePosition(t *testing.T) {
	h := newHub(t)
	h.store(t, 1, 2)
	token := h.add(t, "desk")
	h.store(t, 3, 4)

	for _, step := range []struct {
		body   string
		status int
		want   string
	}{
		{`{"seq":3}`, http.StatusOK, `{"position":3}`},
		{`{"seq":2}`, http.StatusOK, `{"position":3}`},
		{`{"seq":99}`, http.StatusUnprocessableEntity, "seq is beyond the newest event"},
		{`{"seq":-1}`, http.StatusUnprocessableEntity, "body.seq"},
	} {
		got := h.ack(t, token, step.body)
		if got.status != step.status || !strings.Contains(got.body, step.want) {
			t.Errorf("ack %s = %d %s, want %d with %s", step.body, got.status, got.body, step.status, step.want)
		}
	}
}

func TestUnacknowledgedEventsComeBackOnTheNextConnection(t *testing.T) {
	h := newHub(t)
	h.store(t, 1, 2)
	token := h.add(t, "desk")
	h.store(t, 3, 4)

	first := h.open(t, token, "")
	if got := first.untilCaughtUp(t); len(got) != 3 || got[0].id != "3" || got[1].id != "4" {
		t.Fatalf("first connection got %+v, want 3, 4 and caught_up", got)
	}
	if got := h.ack(t, token, `{"seq":3}`); got.status != http.StatusOK {
		t.Fatalf("ack = %d %s", got.status, got.body)
	}
	first.close()

	second := h.open(t, token, "4")
	got := second.untilCaughtUp(t)
	if len(got) != 2 || got[0].id != "4" || got[0].event != "notification" {
		t.Fatalf("second connection got %+v, want 4 and caught_up", got)
	}
	if want := (frame{id: "4", event: "caught_up", data: `{"seq":4,"backlog":1}`}); got[1] != want {
		t.Errorf("got %+v, want %+v", got[1], want)
	}
}

func TestASecondConnectionOfAConsumerReplacesTheFirst(t *testing.T) {
	h := newHub(t)
	token := h.add(t, "desk")

	first := h.open(t, token, "")
	first.untilCaughtUp(t)
	second := h.open(t, token, "")
	second.untilCaughtUp(t)

	if got, want := first.next(t), (frame{event: "replaced", data: "{}"}); got != want {
		t.Fatalf("first connection got %+v, want %+v", got, want)
	}
	first.ended(t)

	h.store(t, 1, 1)
	h.signal.Notify()
	if got := second.next(t); got.id != "1" || got.event != "notification" {
		t.Errorf("second connection got %+v, want notification 1", got)
	}
}

func TestARevokedConsumerIsCutOffAtTheNextPing(t *testing.T) {
	h := newHub(t)
	token := h.add(t, "desk")
	s := h.open(t, token, "")
	s.untilCaughtUp(t)

	if err := h.consumers.Revoke(t.Context(), "desk"); err != nil {
		t.Fatal(err)
	}
	h.ping(t)

	if got := s.next(t); got != (frame{comment: "ping"}) {
		t.Fatalf("got %+v, want the ping", got)
	}
	s.ended(t)
	if got := h.ack(t, token, `{"seq":0}`); got.status != http.StatusUnauthorized || got.header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("ack after the revocation = %d %s, want 401 with WWW-Authenticate: Bearer", got.status, got.body)
	}
}

func TestASeventeenthStreamIsRefused(t *testing.T) {
	h := newHub(t)
	var open []*events
	for i := range 16 {
		s := h.open(t, h.add(t, fmt.Sprintf("desk-%d", i+1)), "")
		s.untilCaughtUp(t)
		open = append(open, s)
	}
	late := h.add(t, "desk-17")

	got := h.call(t, http.MethodGet, streamPath, "", map[string]string{"Authorization": "Bearer " + late})
	if got.status != http.StatusServiceUnavailable || got.header.Get("Retry-After") != "30" || !strings.Contains(got.body, "too many open consumer streams") {
		t.Fatalf("seventeenth stream = %d %s (Retry-After %q), want 503 with Retry-After: 30", got.status, got.body, got.header.Get("Retry-After"))
	}

	open[0].close()
	deadline := time.After(wait)
	retry := time.NewTicker(20 * time.Millisecond)
	defer retry.Stop()
	for !h.accepts(t, late) {
		select {
		case <-retry.C:
		case <-deadline:
			t.Fatalf("no stream was accepted within %s of closing one", wait)
		}
	}
}

func TestTheStreamRefusesAnyMethodButGet(t *testing.T) {
	h := newHub(t)
	token := h.add(t, "desk")
	s := h.open(t, token, "")
	s.untilCaughtUp(t)

	got := h.call(t, http.MethodHead, streamPath, "", map[string]string{"Authorization": "Bearer " + token})
	if got.status != http.StatusMethodNotAllowed || got.header.Get("Allow") != http.MethodGet {
		t.Errorf("HEAD = %d (Allow %q), want 405 with Allow: GET", got.status, got.header.Get("Allow"))
	}

	h.store(t, 1, 1)
	h.signal.Notify()
	if got := s.next(t); got.id != "1" || got.event != "notification" {
		t.Fatalf("the open stream got %+v after the HEAD request, want notification 1", got)
	}
}

func TestAPositionAheadOfTheHubIsMovedBackToTheNewestEvent(t *testing.T) {
	h := newHub(t)
	h.store(t, 1, 5)
	token := h.add(t, "desk")
	if _, err := h.pool.Exec(t.Context(), `UPDATE consumers SET position = 90 WHERE name = 'desk'`); err != nil {
		t.Fatal(err)
	}

	first := h.open(t, token, "")
	if got, want := first.untilCaughtUp(t), (frame{id: "5", event: "caught_up", data: `{"seq":5,"backlog":0}`}); len(got) != 1 || got[0] != want {
		t.Fatalf("first connection got %+v, want only %+v", got, want)
	}
	var position int64
	if err := h.pool.QueryRow(t.Context(), `SELECT position FROM consumers WHERE name = 'desk'`).Scan(&position); err != nil || position != 5 {
		t.Errorf("stored position = %d (err %v), want 5", position, err)
	}
	first.close()

	h.store(t, 6, 7)
	second := h.open(t, token, "")
	got := second.untilCaughtUp(t)
	if len(got) != 3 || got[0].id != "6" || got[1].id != "7" {
		t.Fatalf("second connection got %+v, want 6, 7 and caught_up", got)
	}
	if want := (frame{id: "7", event: "caught_up", data: `{"seq":7,"backlog":2}`}); got[2] != want {
		t.Errorf("got %+v, want %+v", got[2], want)
	}
}
