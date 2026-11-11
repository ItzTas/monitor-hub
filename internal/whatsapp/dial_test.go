package whatsapp

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const handshakeLimit = 50 * time.Millisecond

func TestTheClientTimeoutBoundsOnlyTheHandshake(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		kind, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), kind, data)
	}))
	t.Cleanup(server.Close)
	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{HTTPClient: &http.Client{Timeout: handshakeLimit}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	time.Sleep(2 * handshakeLimit)
	err = conn.Write(t.Context(), websocket.MessageText, []byte("ping"))
	_, echo, readErr := conn.Read(t.Context())

	if err != nil || readErr != nil || string(echo) != "ping" {
		t.Errorf("after the timeout: write %v, read %q, %v", err, echo, readErr)
	}
}

func TestTheClientTimeoutEndsADialNobodyAnswers(t *testing.T) {
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
		close(accepted)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		for conn := range accepted {
			_ = conn.Close()
		}
	})
	started := time.Now()

	_, response, err := websocket.Dial(t.Context(), "ws://"+listener.Addr().String(), &websocket.DialOptions{HTTPClient: &http.Client{Timeout: handshakeLimit}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}

	if err == nil || time.Since(started) > time.Second {
		t.Errorf("Dial = %v after %s", err, time.Since(started))
	}
}
