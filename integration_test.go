package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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
