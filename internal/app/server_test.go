package app

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
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
