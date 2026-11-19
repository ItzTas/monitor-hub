package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/core"
	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

const (
	apiToken  = "api-token-0123456789abcdef"
	feedToken = "feed-token-0123456789abcdef"
	ntfyToken = "tk_ntfy0123456789abcdef"
)

type notification struct {
	title string
	body  string
}

type ntfyInbox struct {
	mu       sync.Mutex
	received []notification
	arrived  chan notification
}

func (b *ntfyInbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	title, err := new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
	if err != nil {
		title = r.Header.Get("Title")
	}
	got := notification{title: title, body: string(body)}
	b.mu.Lock()
	b.received = append(b.received, got)
	b.mu.Unlock()
	select {
	case b.arrived <- got:
	default:
	}
	w.WriteHeader(http.StatusOK)
}

func (b *ntfyInbox) messages() []notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]notification(nil), b.received...)
}

type bench struct {
	t       *testing.T
	environ []string
	dataDir string
	ntfy    *ntfyInbox
}

func newBench(t *testing.T, extra ...string) *bench {
	t.Helper()
	monitorsDir := t.TempDir()
	installTestmonitor(t, filepath.Join(monitorsDir, "testmonitor"))
	b := &bench{t: t, dataDir: t.TempDir(), ntfy: &ntfyInbox{arrived: make(chan notification, 16)}}
	ntfy := httptest.NewServer(b.ntfy)
	t.Cleanup(ntfy.Close)
	b.environ = append([]string{
		"DATABASE_URL=" + coretest.Database(t),
		"API_TOKEN=" + apiToken,
		"FEED_TOKEN=" + feedToken,
		"MONITORS_DIR=" + monitorsDir,
		"DATA_DIR=" + b.dataDir,
		"HTTP_ADDRESS=127.0.0.1:0",
		"NTFY_URL=" + ntfy.URL + "/hub",
		"NTFY_TOKEN=" + ntfyToken,
		"PATH=" + os.Getenv("PATH"),
		"TESTMONITOR__EVENTS=2",
	}, extra...)
	return b
}

func installTestmonitor(t *testing.T, dir string) {
	t.Helper()
	binary, err := os.ReadFile(testmonitorBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"protocol_version": 2, "name": "testmonitor", "command": ["testmonitor"]}`
	if err := os.WriteFile(filepath.Join(dir, "monitor.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testmonitor"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (b *bench) assemble(logger *slog.Logger) *application {
	b.t.Helper()
	cfg, err := core.Load(b.environ, os.ReadFile)
	if err != nil {
		b.t.Fatal(err)
	}
	app, err := assemble(b.t.Context(), cfg, b.environ, logger, time.Now)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(app.Close)
	return app
}

func (b *bench) setEvents(count string) {
	b.t.Helper()
	path := filepath.Join(b.dataDir, "monitors", "testmonitor", "events")
	if err := os.WriteFile(path, []byte(count), 0o600); err != nil {
		b.t.Fatal(err)
	}
}

func startSupervisor(t *testing.T, app *application) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.supervisor.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the supervisor returned %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("the supervisor did not stop within 15s")
		}
	})
	waitForMonitor(t, app)
}

func waitForMonitor(t *testing.T, app *application) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := app.supervisor.Client("testmonitor"); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the test monitor did not start within 15s")
}

func request(t *testing.T, app *application, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, app, method, target, token, nil)
}

func send(t *testing.T, app *application, method, target, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.handler.ServeHTTP(rec, req)
	return rec
}

func eventTitles(t *testing.T, app *application) []string {
	t.Helper()
	rec := request(t, app, http.MethodGet, "/api/events", apiToken)
	var page struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("events = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	titles := []string{}
	for _, item := range page.Items {
		titles = append(titles, item.Title)
	}
	return titles
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type logMessages struct {
	messages chan string
}

func newLogMessages() *logMessages {
	return &logMessages{messages: make(chan string, 256)}
}

func (l *logMessages) Enabled(context.Context, slog.Level) bool {
	return true
}

func (l *logMessages) Handle(_ context.Context, r slog.Record) error {
	select {
	case l.messages <- r.Message:
	default:
	}
	return nil
}

func (l *logMessages) WithAttrs([]slog.Attr) slog.Handler {
	return l
}

func (l *logMessages) WithGroup(string) slog.Handler {
	return l
}

func (l *logMessages) waitFor(t *testing.T, message string, limit time.Duration) {
	t.Helper()
	deadline := time.After(limit)
	for {
		select {
		case got := <-l.messages:
			if got == message {
				return
			}
		case <-deadline:
			t.Fatalf("no %q log line within %s", message, limit)
		}
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

type consumerStream struct {
	frames chan frame
	stop   chan struct{}
}

func openStream(t *testing.T, base, token string) *consumerStream {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/consumer/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream = %d %s, want 200", resp.StatusCode, body)
	}
	s := &consumerStream{frames: make(chan frame), stop: make(chan struct{})}
	go s.read(resp.Body)
	t.Cleanup(func() { close(s.stop) })
	return s
}

func (s *consumerStream) read(body io.Reader) {
	defer close(s.frames)
	scanner := bufio.NewScanner(body)
	current := frame{}
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			current.set(line)
			continue
		}
		select {
		case s.frames <- current:
		case <-s.stop:
			return
		}
		current = frame{}
	}
}

func (s *consumerStream) next(t *testing.T) frame {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			t.Fatal("the stream ended while a message was expected")
		}
		return f
	case <-time.After(15 * time.Second):
		t.Fatal("no stream message within 15s")
	}
	return frame{}
}

func (s *consumerStream) ended(t *testing.T) {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			t.Fatalf("got %+v, want the stream to end", f)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stream did not end within 15s")
	}
}

func fieldNames(t *testing.T, object string) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(object))
	if _, err := decoder.Token(); err != nil {
		t.Fatalf("%s: %v", object, err)
	}
	var names []string
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			t.Fatalf("%s: %v", object, err)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("%s: %v", object, err)
		}
		names = append(names, fmt.Sprint(name))
	}
	return names
}
