package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/internal/core"
	"github.com/ItzTas/monitor-hub/internal/core/coretest"
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
}

func (b *ntfyInbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	title, err := new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
	if err != nil {
		title = r.Header.Get("Title")
	}
	b.mu.Lock()
	b.received = append(b.received, notification{title: title, body: string(body)})
	b.mu.Unlock()
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
	pluginsDir := t.TempDir()
	installTestplugin(t, filepath.Join(pluginsDir, "testplugin"))
	b := &bench{t: t, dataDir: t.TempDir(), ntfy: &ntfyInbox{}}
	ntfy := httptest.NewServer(b.ntfy)
	t.Cleanup(ntfy.Close)
	b.environ = append([]string{
		"DATABASE_URL=" + coretest.Database(t),
		"API_TOKEN=" + apiToken,
		"FEED_TOKEN=" + feedToken,
		"PLUGINS_DIR=" + pluginsDir,
		"DATA_DIR=" + b.dataDir,
		"HTTP_ADDRESS=127.0.0.1:0",
		"NTFY_URL=" + ntfy.URL + "/hub",
		"NTFY_TOKEN=" + ntfyToken,
		"PATH=" + os.Getenv("PATH"),
		"TESTPLUGIN__EVENTS=2",
	}, extra...)
	return b
}

func installTestplugin(t *testing.T, dir string) {
	t.Helper()
	binary, err := os.ReadFile(testpluginBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"protocol_version": 1, "name": "testplugin", "command": ["testplugin"]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testplugin"), binary, 0o755); err != nil {
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
	path := filepath.Join(b.dataDir, "plugins", "testplugin", "events")
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
	waitForPlugin(t, app)
}

func waitForPlugin(t *testing.T, app *application) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := app.supervisor.Client("testplugin"); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the test plugin did not start within 15s")
}

func request(t *testing.T, app *application, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
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
