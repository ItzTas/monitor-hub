package app

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamsCanExtendTheirWriteDeadline(t *testing.T) {
	results := make(chan [2]error, 1)
	handler := withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		controller := http.NewResponseController(w)
		results <- [2]error{controller.SetWriteDeadline(time.Now().Add(time.Minute)), controller.Flush()}
	}), slog.New(slog.DiscardHandler))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/consumer/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	select {
	case got := <-results:
		if got[0] != nil || got[1] != nil {
			t.Errorf("SetWriteDeadline = %v, Flush = %v; want both nil", got[0], got[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not run within 5s")
	}
}

func TestRequestLogsShowThePathWithoutTheQuery(t *testing.T) {
	const token = "mht_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	var logs bytes.Buffer
	handler := withMiddleware(http.NotFoundHandler(), slog.New(slog.NewTextHandler(&logs, nil)))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/feed.xml?monitor=sei&token="+token, nil))

	if !strings.Contains(logs.String(), "path=/feed.xml status=404") {
		t.Errorf("the request log misses the path and status")
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "monitor=sei") {
		t.Error("the request log shows the query string, token included")
	}
}

func TestBodyLimit(t *testing.T) {
	cases := map[string]int64{
		"/api/monitors/sei/settings":          131072,
		"/api/monitors/sei/check":             1024,
		"/api/monitors/sei/settings/progress": 1024,
		"/api/monitors/a/b/settings":          1024,
		"/api/session":                        1024,
	}
	for path, want := range cases {
		if got := bodyLimit(path); got != want {
			t.Errorf("bodyLimit(%q) = %d, want %d", path, got, want)
		}
	}
}

func TestTheMiddlewareLimitsEachBodyByItsPath(t *testing.T) {
	var read int
	var readErr error
	handler := withMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body []byte
		body, readErr = io.ReadAll(r.Body)
		read = len(body)
	}), slog.New(slog.DiscardHandler))
	body := strings.Repeat("x", 2048)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/monitors/sei/settings", strings.NewReader(body)))
	if readErr != nil || read != len(body) {
		t.Errorf("a configuration save read %d bytes (%v), want all %d", read, readErr, len(body))
	}

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/session", strings.NewReader(body)))
	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) || tooLarge.Limit != maxBodyBytes {
		t.Errorf("a sign-in read %d bytes with %v, want a MaxBytesError at %d bytes", read, readErr, maxBodyBytes)
	}
}
