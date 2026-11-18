package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/icons"
)

func TestHubEndToEnd(t *testing.T) {
	b := newBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	startSupervisor(t, app)
	ctx := t.Context()

	app.ingest.PollOnce(ctx)
	app.dispatcher.RunOnce(ctx)

	if titles := eventTitles(t, app); strings.Join(titles, ",") != "ping 2,ping 1" {
		t.Errorf("events after the baseline = %q, want ping 2, ping 1", titles)
	}
	if got := b.ntfy.messages(); len(got) != 0 {
		t.Errorf("the baseline sent %v, want nothing", got)
	}

	b.setEvents("3")
	app.ingest.PollOnce(ctx)
	app.dispatcher.RunOnce(ctx)

	want := []notification{{title: "monitor-hub: 1 update from Test plugin", body: "ping 3"}}
	if got := b.ntfy.messages(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("ntfy received %+v, want %+v", got, want)
	}
	feed := request(t, app, http.MethodGet, "/feed.xml?token="+feedToken, "")
	if feed.Code != http.StatusOK || !strings.Contains(feed.Body.String(), "ping 3") {
		t.Errorf("feed = %d, want it to list ping 3:\n%s", feed.Code, feed.Body.String())
	}
	plugins := request(t, app, http.MethodGet, "/api/plugins", apiToken)
	for _, part := range []string{`"name":"testplugin"`, `"display_name":"Test plugin"`, `"running":true`, `"state":"idle"`} {
		if !strings.Contains(plugins.Body.String(), part) {
			t.Errorf("plugins = %d %s, want it to contain %s", plugins.Code, plugins.Body.String(), part)
		}
	}
	check := request(t, app, http.MethodPost, "/api/plugins/testplugin/check", apiToken)
	if check.Code != http.StatusOK || strings.TrimSpace(check.Body.String()) != `{"accepted":true,"message":""}` {
		t.Errorf("check = %d %s", check.Code, check.Body.String())
	}
	if health := request(t, app, http.MethodGet, "/healthz", ""); health.Code != http.StatusOK {
		t.Errorf("health = %d", health.Code)
	}
}

func TestRunStopsEverythingAndLogsNoSecret(t *testing.T) {
	b := newBench(t)
	logs := &syncBuffer{}
	app := b.assemble(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	waitForPlugin(t, app)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s of the cancellation")
	}

	if _, ok := app.supervisor.Client("testplugin"); ok {
		t.Error("the plugin is still connected after Run returned")
	}
	for _, part := range []string{"monitor-hub starting", "plugins=testplugin", "channels=ntfy", "plugin=testplugin"} {
		if !strings.Contains(logs.String(), part) {
			t.Errorf("the logs miss %q:\n%s", part, logs.String())
		}
	}
	for _, secret := range []string{apiToken, feedToken, ntfyToken} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the logs contain %q:\n%s", secret, logs.String())
		}
	}
}

func TestHubNotifiesAsSoonAsThePluginSignals(t *testing.T) {
	b := newBench(t, "TESTPLUGIN__WATCH=follow")
	logs := newLogMessages()
	app := b.assemble(slog.New(logs))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	logs.waitFor(t, "baseline complete", 15*time.Second)

	b.setEvents("3")
	written := time.Now()
	want := notification{title: "monitor-hub: 1 update from Test plugin", body: "ping 3"}
	select {
	case got := <-b.ntfy.arrived:
		t.Logf("the notification arrived %s after the event was written", time.Since(written))
		if got != want {
			t.Errorf("ntfy received %+v, want %+v", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the notification did not arrive within 10s: the hub polls a plugin without Watch every 15s and the dispatcher every 30s, so the signal was not followed")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s of the cancellation")
	}
}

func TestRunFailsWhenTheAddressIsTaken(t *testing.T) {
	var config net.ListenConfig
	taken, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	b := newBench(t, "HTTP_ADDRESS="+taken.Addr().String())
	app := b.assemble(slog.New(slog.DiscardHandler))

	done := make(chan error, 1)
	go func() { done <- app.Run(t.Context()) }()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), taken.Addr().String()) {
			t.Errorf("Run returned %v, want an error naming %s", err, taken.Addr())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run kept going with its address taken")
	}
	if _, ok := app.supervisor.Client("testplugin"); ok {
		t.Error("a plugin runs although the hub could not serve")
	}
}

func TestAConsumerReceivesEventsAfterTheBaseline(t *testing.T) {
	b := newBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	startSupervisor(t, app)
	server := httptest.NewServer(app.handler)
	t.Cleanup(server.Close)
	t.Cleanup(app.signal.Close)
	ctx := t.Context()

	app.ingest.PollOnce(ctx)
	token, err := consumers.NewService(app.pool).Add(ctx, "desk")
	if err != nil {
		t.Fatal(err)
	}
	stream := openStream(t, server.URL, token)
	if got, want := stream.next(t), (frame{id: "2", event: "caught_up", data: `{"seq":2,"backlog":0}`}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	list := request(t, app, http.MethodGet, "/api/consumers", apiToken)
	var listed struct {
		Items []struct {
			Name      string `json:"name"`
			Connected bool   `json:"connected"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || list.Code != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].Name != "desk" || !listed.Items[0].Connected {
		t.Errorf("consumers with the stream open = %d %s (%v), want desk connected", list.Code, list.Body.String(), err)
	}

	b.setEvents("3")
	app.ingest.PollOnce(ctx)

	got := stream.next(t)
	if got.id != "3" || got.event != "notification" {
		t.Fatalf("got %+v, want notification 3", got)
	}
	icon := icons.Default().Image
	fields := []string{"seq", "monitor", "display_name", "kind", "urgent", "title", "body", "url", "occurred_at", "detected_at", "icon", "monitor_icon"}
	if names := fieldNames(t, got.data); !slices.Equal(names, fields) {
		t.Errorf("notification fields = %q, want %q", names, fields)
	}
	bell := `{"emoji":"🔔","image":"` + icon + `"}`
	for _, part := range []string{`"monitor":"testplugin"`, `"title":"ping 3"`, `"urgent":false`, `"icon":` + bell, `"monitor_icon":` + bell} {
		if !strings.Contains(got.data, part) {
			t.Errorf("notification %s, want it to contain %s", got.data, part)
		}
	}
	ack := send(t, app, http.MethodPost, "/consumer/v1/ack", token, strings.NewReader(`{"seq":3}`))
	if ack.Code != http.StatusOK || strings.TrimSpace(ack.Body.String()) != `{"position":3}` {
		t.Errorf("ack = %d %s, want 200 {\"position\":3}", ack.Code, ack.Body.String())
	}
	if image := request(t, app, http.MethodGet, icon, ""); image.Code != http.StatusOK {
		t.Errorf("GET %s = %d, want 200", icon, image.Code)
	}
}

func TestRunEndsOpenStreams(t *testing.T) {
	var config net.ListenConfig
	reserved, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	b := newBench(t, "HTTP_ADDRESS="+address)
	logs := &syncBuffer{}
	app := b.assemble(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	token, err := consumers.NewService(app.pool).Add(t.Context(), "desk")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	waitForPlugin(t, app)
	stream := openStream(t, "http://"+address, token)
	if got := stream.next(t); got.event != "caught_up" {
		t.Fatalf("got %+v, want caught_up", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s of the cancellation")
	}
	stream.ended(t)
	if strings.Contains(logs.String(), token) {
		t.Errorf("the logs contain the consumer token:\n%s", logs.String())
	}
}
