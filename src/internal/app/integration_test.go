package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/ItzTas/monitor-hub/sdk-go"
	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts/accountstest"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/icons"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

func TestHubEndToEnd(t *testing.T) {
	b := newBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	startSupervisor(t, app)
	ctx := t.Context()

	token := adminToken(t, app)
	app.ingest.PollOnce(ctx)
	app.dispatcher.RunOnce(ctx)

	if titles := eventTitles(t, app, token); strings.Join(titles, ",") != "ping 2,ping 1" {
		t.Errorf("events after the baseline = %q, want ping 2, ping 1", titles)
	}
	if got := b.ntfy.messages(); len(got) != 0 {
		t.Errorf("the baseline sent %v, want nothing", got)
	}

	b.setEvents("3")
	app.ingest.PollOnce(ctx)
	app.dispatcher.RunOnce(ctx)

	want := []notification{{title: "monitor-hub: 1 update from Test monitor", body: "ping 3"}}
	if got := b.ntfy.messages(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("ntfy received %+v, want %+v", got, want)
	}
	feed := request(t, app, http.MethodGet, "/feed.xml?token="+token, "")
	if feed.Code != http.StatusOK || !strings.Contains(feed.Body.String(), "ping 3") {
		t.Errorf("feed = %d, want it to list ping 3:\n%s", feed.Code, feed.Body.String())
	}
	monitors := request(t, app, http.MethodGet, "/api/monitors", token)
	for _, part := range []string{`"name":"testmonitor"`, `"display_name":"Test monitor"`, `"running":true`, `"state":"idle"`} {
		if !strings.Contains(monitors.Body.String(), part) {
			t.Errorf("monitors = %d %s, want it to contain %s", monitors.Code, monitors.Body.String(), part)
		}
	}
	check := request(t, app, http.MethodPost, "/api/monitors/testmonitor/check", token)
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
	waitForMonitor(t, app)
	token := adminToken(t, app)
	cookie := signIn(t, app)
	if feed := request(t, app, http.MethodGet, "/feed.xml?token="+token, ""); feed.Code != http.StatusOK {
		t.Errorf("feed = %d, want 200", feed.Code)
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

	secrets := map[string]string{
		"the personal token": token,
		"the password":       accountstest.Password,
		"the session cookie": cookie.Value,
		"the ntfy token":     ntfyToken,
	}
	redacted := logs.String()
	for name, secret := range secrets {
		if strings.Contains(redacted, secret) {
			t.Errorf("the logs contain %s", name)
		}
		redacted = strings.ReplaceAll(redacted, secret, "<"+name+">")
	}
	if _, ok := app.supervisor.Client("testmonitor"); ok {
		t.Error("the monitor is still connected after Run returned")
	}
	for _, part := range []string{"monitor-hub starting", "monitors=testmonitor", "channels=ntfy", "monitor=testmonitor", "path=/feed.xml status=200"} {
		if !strings.Contains(redacted, part) {
			t.Errorf("the logs miss %q:\n%s", part, redacted)
		}
	}
}

func TestAPersonalTokenLosesWhatItsOwnerLoses(t *testing.T) {
	b := newBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	accountstest.Person(t, app.accounts, "ana-lima", "monitor:*:view")
	token := accountstest.Token(t, app.accounts, "ana-lima", "monitor:*:view")
	cookie := signIn(t, app)

	if got := monitorNames(t, app, token); !slices.Equal(got, []string{"testmonitor"}) {
		t.Fatalf("the token of ana-lima lists %v, want testmonitor", got)
	}
	if rec := sendSigned(t, app, http.MethodPut, "/api/users/ana-lima/permissions", cookie, strings.NewReader(`{"permissions":[]}`)); rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/users/ana-lima/permissions = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := monitorNames(t, app, token); len(got) != 0 {
		t.Errorf("the token of ana-lima lists %v after she lost her permissions, want nothing", got)
	}
}

func monitorNames(t *testing.T, app *application, token string) []string {
	t.Helper()
	rec := request(t, app, http.MethodGet, "/api/monitors", token)
	var listed struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("monitors = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	names := []string{}
	for _, item := range listed.Items {
		names = append(names, item.Name)
	}
	return names
}

func TestHubNotifiesAsSoonAsTheMonitorSignals(t *testing.T) {
	b := newBench(t)
	logs := newLogMessages()
	app := b.assemble(slog.New(logs))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	logs.waitFor(t, "baseline complete", 15*time.Second)

	b.setEvents("3")
	written := time.Now()
	want := notification{title: "monitor-hub: 1 update from Test monitor", body: "ping 3"}
	select {
	case got := <-b.ntfy.arrived:
		t.Logf("the notification arrived %s after the event was written", time.Since(written))
		if got != want {
			t.Errorf("ntfy received %+v, want %+v", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the notification did not arrive within 10s: the hub polls a monitor whose Watch is down every 15s and the dispatcher every 30s, so the signal was not followed")
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
	if _, ok := app.supervisor.Client("testmonitor"); ok {
		t.Error("a monitor runs although the hub could not serve")
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
	list := sendSigned(t, app, http.MethodGet, "/api/consumers", signIn(t, app), nil)
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
	for _, part := range []string{`"monitor":"testmonitor"`, `"title":"ping 3"`, `"urgent":false`, `"icon":` + bell, `"monitor_icon":` + bell} {
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
	waitForMonitor(t, app)
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

const (
	testmonitorSettings = `{"settings_version":2,"fields":[` +
		`{"name":"events","label":"Events","type":"integer","default":0,"env":"EVENTS"},` +
		`{"name":"password","label":"Password","type":"string","secret":true,"env":"PASSWORD"}]}`
	savedPassword = "hunter2-secret-value"
	envPassword   = "env-password-value"
)

// newConfiguredBench installs the test monitor with testmonitorSettings and a validator that
// refuses a negative EVENTS.
func newConfiguredBench(t *testing.T, extra ...string) *bench {
	t.Helper()
	b := newBench(t, extra...)
	installTestmonitor(t, filepath.Join(b.monitorsDir, "testmonitor"), testmonitorSettings, "events")
	return b
}

func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

// startedEvents is the EVENTS of the last start of the test monitor, and its pid.
func startedEvents(t *testing.T, b *bench) (events, pid string) {
	t.Helper()
	starts := b.starts()
	if len(starts) == 0 {
		t.Fatal("the test monitor never started")
	}
	pid, events, _ = strings.Cut(starts[len(starts)-1], " ")
	return events, pid
}

func TestSavingTheConfigurationRestartsTheMonitorWithIt(t *testing.T) {
	b := newConfiguredBench(t)
	logs := &syncBuffer{}
	app := b.assemble(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	stop := run(t, app)
	waitForMonitor(t, app)
	cookie := signIn(t, app)
	var answers []string
	answered := func(rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		answers = append(answers, rec.Body.String())
		return rec
	}
	const url = "/api/monitors/testmonitor/settings"

	before := answered(sendSigned(t, app, http.MethodGet, url, cookie, nil)).Body.String()
	if !strings.Contains(before, `"document":{"events":2}`) || !strings.Contains(before, `{"name":"events","source":"env","saved":false,"env_set":true}`) {
		t.Errorf("GET settings = %s, want events 2 from the .env", withoutPasswords(before))
	}

	save := answered(putSettings(t, app, cookie, `{"document":{"events":3},"secrets":{"password":"`+savedPassword+`"}}`))
	if save.Code != http.StatusAccepted {
		t.Fatalf("PUT settings = %d %s, want 202", save.Code, shown(t, save))
	}
	waitProgress(t, app, cookie, monitorconfig.ProgressUp)
	if events, _ := startedEvents(t, b); events != "3" {
		t.Errorf("the monitor started with EVENTS %q, want 3", events)
	}
	after := answered(sendSigned(t, app, http.MethodGet, url, cookie, nil)).Body.String()
	if !strings.Contains(after, `"secrets":[{"path":"password","set":true,"saved":true,"env_set":false}]`) {
		t.Errorf("GET settings after the save = %s, want the password set", withoutPasswords(after))
	}
	starts := b.starts()

	refused := []struct {
		name, body, want string
	}{
		{"text", `{"document":{"events":"x"}}`, `{"problems":[{"path":"events","message":"use a whole number"}]}`},
		{"negative", `{"document":{"events":-1}}`, `{"problems":[{"path":"events","message":"use a whole number of 0 or more"}]}`},
	}
	for _, tc := range refused {
		rec := answered(putSettings(t, app, cookie, tc.body))
		if rec.Code != http.StatusUnprocessableEntity || !sameJSON(t, rec.Body.String(), tc.want) {
			t.Errorf("PUT %s = %d %s, want 422 %s", tc.name, rec.Code, shown(t, rec), tc.want)
		}
	}
	token := adminToken(t, app)
	if rec := answered(send(t, app, http.MethodPut, url, token, strings.NewReader(`{"document":{"events":5}}`))); rec.Code != http.StatusForbidden {
		t.Errorf("PUT with a personal token = %d %s, want 403", rec.Code, shown(t, rec))
	}
	accountstest.Person(t, app.accounts, "testmonitor-viewer", "monitor:testmonitor:view", "monitor:testmonitor:check")
	viewer := &http.Cookie{Name: gate.CookieName, Value: accountstest.Session(t, app.accounts, "testmonitor-viewer")}
	if rec := answered(sendSigned(t, app, http.MethodGet, url, viewer, nil)); rec.Code != http.StatusForbidden {
		t.Errorf("GET by a person without configure = %d %s, want 403", rec.Code, shown(t, rec))
	}
	if rec := answered(putSettings(t, app, viewer, `{"document":{"events":5}}`)); rec.Code != http.StatusForbidden {
		t.Errorf("PUT by a person without configure = %d %s, want 403", rec.Code, shown(t, rec))
	}
	if got := b.starts(); !slices.Equal(got, starts) {
		t.Errorf("the monitor started %d times after the refused saves, want none", len(got)-len(starts))
	}

	stop()
	secrets := map[string]string{
		"the saved password":       savedPassword,
		"the password of a person": accountstest.Password,
		"a session cookie":         cookie.Value,
		"the viewer's cookie":      viewer.Value,
		"the personal token":       token,
	}
	for name, secret := range secrets {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the logs contain %s", name)
		}
		for i, answer := range answers {
			if strings.Contains(answer, secret) {
				t.Errorf("answer %d contains %s", i, name)
			}
		}
	}
}

func TestTheDocumentNeverTouchesTheDisk(t *testing.T) {
	tmp := t.TempDir()
	b := newConfiguredBench(t, "TESTMONITOR__RECORD_FD=1", "TESTMONITOR__PASSWORD="+envPassword)
	t.Setenv("TMPDIR", tmp)
	app := b.assemble(slog.New(slog.DiscardHandler))
	run(t, app)
	waitForMonitor(t, app)
	cookie := signIn(t, app)

	if rec := putSettings(t, app, cookie, `{"secrets":{"password":"`+savedPassword+`"}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("PUT settings = %d %s, want 202", rec.Code, shown(t, rec))
	}
	waitProgress(t, app, cookie, monitorconfig.ProgressUp)

	monitorData := filepath.Join(b.dataDir, "monitors", "testmonitor")
	fd, err := os.ReadFile(filepath.Join(monitorData, "fd"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(fd), "/memfd:monitor-hub-config") {
		t.Errorf("the document came on %q, want a /memfd:monitor-hub-config descriptor", fd)
	}
	secrets := map[string]string{"the saved password": savedPassword, "the password from the .env": envPassword}
	_, pid := startedEvents(t, b)
	environ, err := os.ReadFile("/proc/" + pid + "/environ")
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range secrets {
		if bytes.Contains(environ, []byte(secret)) {
			t.Errorf("the monitor's environment holds %s", name)
		}
	}
	for _, root := range []string{b.dataDir, tmp} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() {
				return err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for name, secret := range secrets {
				if bytes.Contains(content, []byte(secret)) {
					t.Errorf("%s holds %s", path, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(b.monitorsDir, "testmonitor", "validator-mode"), []byte("report-env"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := putSettings(t, app, cookie, `{"document":{"events":5}}`)
	var answer struct {
		Problems []struct {
			Message string `json:"message"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusUnprocessableEntity || len(answer.Problems) != 1 {
		t.Fatalf("PUT with the validator reporting its environment = %d %s (%v)", rec.Code, shown(t, rec), err)
	}
	names := strings.Split(answer.Problems[0].Message, ",")
	if slices.Contains(names, "PASSWORD") || !slices.Contains(names, "MONITOR_HUB_CONFIG_FD") {
		t.Errorf("the validator received the variables %v, want MONITOR_HUB_CONFIG_FD and no PASSWORD", names)
	}
}

func TestAnEnvValueThatDoesNotConvertStopsTheMonitor(t *testing.T) {
	b := newConfiguredBench(t, "TESTMONITOR__EVENTS=three")
	logs := &jsonLogs{}
	app := b.assemble(slog.New(slog.NewJSONHandler(logs, nil)))
	run(t, app)

	const problem = `EVENTS is not an integer ("three")`
	if record := logs.waitRecord(t, "monitor failed to start"); record["monitor"] != "testmonitor" || record["error"] != "events: "+problem {
		t.Errorf("the failed start was logged as %v, want monitor testmonitor and error events: %s", withoutPasswords(fmt.Sprint(record)), problem)
	}
	cookie := signIn(t, app)
	problems, _ := json.Marshal(readSettings(t, app, cookie)["problems"])
	if want := `[{"path":"events","message":` + strconv.Quote(problem) + `}]`; !sameJSON(t, string(problems), want) {
		t.Errorf("GET settings problems = %s, want %s", withoutPasswords(string(problems)), want)
	}
	if starts := b.starts(); len(starts) != 0 {
		t.Errorf("the monitor started %d times with text that does not convert, want none", len(starts))
	}

	if rec := putSettings(t, app, cookie, `{"document":{"events":4}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("PUT settings = %d %s, want 202", rec.Code, shown(t, rec))
	}
	waitProgress(t, app, cookie, monitorconfig.ProgressUp)
	if events, _ := startedEvents(t, b); events != "4" {
		t.Errorf("the monitor started with EVENTS %q, want 4", events)
	}
}

func TestAFailedStartAfterASaveShowsItsReason(t *testing.T) {
	b := newConfiguredBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	run(t, app)
	waitForMonitor(t, app)
	cookie := signIn(t, app)

	line := `{"@level":"error","@message":"fatal","error":"connecting to the database: refused"}`
	body, err := json.Marshal(map[string]any{"variables": map[string]any{"set": map[string]string{"EXIT_BEFORE_HANDSHAKE": "1", "STDERR_LINES": line}}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := putSettings(t, app, cookie, string(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("PUT settings = %d %s, want 202", rec.Code, shown(t, rec))
	}
	if p := waitProgress(t, app, cookie, monitorconfig.ProgressFailed); p["reason"] != "connecting to the database: refused" {
		t.Errorf("the failed restart has the reason %q, want connecting to the database: refused", p["reason"])
	}

	if rec := putSettings(t, app, cookie, `{"variables":{"unset":["EXIT_BEFORE_HANDSHAKE","STDERR_LINES"]}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("PUT settings = %d %s, want 202", rec.Code, shown(t, rec))
	}
	waitProgress(t, app, cookie, monitorconfig.ProgressUp)
}

func TestARestartedMonitorKeepsNotifying(t *testing.T) {
	b := newConfiguredBench(t, "TESTMONITOR__WATCH=follow")
	logs := newLogMessages()
	app := b.assemble(slog.New(logs))
	run(t, app)
	logs.waitFor(t, "baseline complete", 15*time.Second)
	cookie := signIn(t, app)

	if rec := putSettings(t, app, cookie, `{"variables":{"set":{"NOTE":"x"}}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("PUT settings = %d %s, want 202", rec.Code, shown(t, rec))
	}
	waitProgress(t, app, cookie, monitorconfig.ProgressUp)

	b.setEvents("3")
	want := notification{title: "monitor-hub: 1 update from Test monitor", body: "ping 3"}
	select {
	case got := <-b.ntfy.arrived:
		if got != want {
			t.Errorf("ntfy received %+v, want %+v", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the notification did not arrive within 10s of the event: the hub did not follow the restarted monitor's signal")
	}
}

func TestTheDocumentIsDeliveredBeforeAnyoneSignsIn(t *testing.T) {
	b := newConfiguredBench(t, "TESTMONITOR__RECORD_CONFIG=1")
	app := b.assemble(slog.New(slog.DiscardHandler))
	run(t, app)
	waitForMonitor(t, app)

	config, err := os.ReadFile(filepath.Join(b.dataDir, "monitors", "testmonitor", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(config) != `{"events":2}` {
		t.Errorf("the monitor received the document %s, want {\"events\":2}", config)
	}
	if rec := request(t, app, http.MethodGet, "/api/monitors/testmonitor/settings", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET settings without a session = %d %s, want 401", rec.Code, shown(t, rec))
	}
}

func TestAMonitorWithoutSettingsBehavesAsBefore(t *testing.T) {
	b := newBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	run(t, app)
	waitForMonitor(t, app)

	_, pid := startedEvents(t, b)
	environ, err := os.ReadFile("/proc/" + pid + "/environ")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for kv := range strings.SplitSeq(strings.TrimSuffix(string(environ), "\x00"), "\x00") {
		name, _, _ := strings.Cut(kv, "=")
		// go-plugin adds its own variables to every monitor it starts.
		if !strings.HasPrefix(name, "PLUGIN_") && name != sdk.Handshake.MagicCookieKey {
			got = append(got, kv)
		}
	}
	slices.Sort(got)
	want := []string{
		"EVENTS=2",
		"MONITOR_HUB_DATA_DIR=" + filepath.Join(b.dataDir, "monitors", "testmonitor"),
		"PATH=" + os.Getenv("PATH"),
		"RECORD_START=EVENTS",
		"TMPDIR=/tmp",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the monitor's environment = %q, want %q", got, want)
	}
	links, err := filepath.Glob("/proc/" + pid + "/fd/*")
	if err != nil || len(links) == 0 {
		t.Fatalf("the monitor's descriptors could not be listed (%d found, %v)", len(links), err)
	}
	for _, link := range links {
		target, err := os.Readlink(link)
		if err == nil && strings.HasPrefix(target, "/memfd:monitor-hub-config") {
			t.Errorf("descriptor %s of the monitor is %s, want no configuration document", filepath.Base(link), target)
		}
	}

	cookie := signIn(t, app)
	if settings := readSettings(t, app, cookie); settings["declared"] != false {
		t.Errorf("declared = %v, want false", settings["declared"])
	}
	rec := putSettings(t, app, cookie, `{"document":{"events":3}}`)
	want422 := `{"problems":[{"path":"","message":"the monitor ships no readable settings.json version 2, so its configuration cannot be changed here"}]}`
	if rec.Code != http.StatusUnprocessableEntity || !sameJSON(t, rec.Body.String(), want422) {
		t.Errorf("PUT settings = %d %s, want 422 %s", rec.Code, shown(t, rec), want422)
	}
}

func TestAnUnsupportedSettingsVersionIsLoggedAndIgnored(t *testing.T) {
	b := newBench(t)
	installTestmonitor(t, filepath.Join(b.monitorsDir, "testmonitor"), `{"settings_version":3,"fields":[]}`, "")
	logs := &jsonLogs{}
	app := b.assemble(slog.New(slog.NewJSONHandler(logs, nil)))
	run(t, app)
	waitForMonitor(t, app)

	record := logs.waitRecord(t, "the monitor's settings.json is ignored; none of its configuration can be changed here")
	if text, _ := record["error"].(string); record["monitor"] != "testmonitor" || !strings.Contains(text, "settings.json: settings_version is 3, want 2") {
		t.Errorf("the ignored settings.json was logged as %v, want monitor testmonitor and the version error", withoutPasswords(fmt.Sprint(record)))
	}
	if settings := readSettings(t, app, signIn(t, app)); settings["declared"] != false {
		t.Errorf("declared = %v, want false", settings["declared"])
	}
}

const (
	pagesSettings = `{"settings_version":2,"fields":[` +
		`{"name":"events","label":"Events","type":"integer","default":0,"env":"EVENTS"},` +
		`{"name":"password","label":"Password","type":"string","secret":true,"env":"PASSWORD"},` +
		`{"name":"labels","label":"Labels","type":"list","items":{"type":"string"}}]}`
	wrongPassword = "not the password of paulo-santos"
	textRunning   = "The monitor is running with the new values."
	textEnvBroken = "The monitor cannot start because these values in the hub's .env file do not fit their fields. Set them on this page, or fix the .env file and restart the hub:"
)

// newPagesBench installs the test monitor with pagesSettings and a validator that refuses a
// negative EVENTS.
func newPagesBench(t *testing.T, extra ...string) *bench {
	t.Helper()
	b := newBench(t, extra...)
	installTestmonitor(t, filepath.Join(b.monitorsDir, "testmonitor"), pagesSettings, "events")
	return b
}

// text is the body of a page as a person reads it, the passwords of these tests taken out.
func text(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return html.UnescapeString(shown(t, rec))
}

func TestTheSettingsPageChangesAMonitor(t *testing.T) {
	b := newPagesBench(t)
	logs := &syncBuffer{}
	app := b.assemble(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	stop := run(t, app)
	waitForMonitor(t, app)
	ensureAdmin(t, app)
	cookie := pageSignIn(t, app, "paulo-santos", accountstest.Password)
	var answers []*httptest.ResponseRecorder
	page := func(method, target string, form url.Values, htmx bool) *httptest.ResponseRecorder {
		rec := pageRequest(t, app, method, target, form, cookie, htmx)
		answers = append(answers, rec)
		return rec
	}

	if rec := page(http.MethodGet, "/settings", nil, false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `href="/settings/testmonitor"`) {
		t.Errorf("GET /settings = %d, want 200 listing testmonitor:\n%s", rec.Code, shown(t, rec))
	}
	rec := page(http.MethodGet, "/settings/testmonitor", nil, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="f-events" name="v:events" type="text" value="2"`) || !strings.Contains(rec.Body.String(), "From .env") {
		t.Fatalf("GET /settings/testmonitor = %d, want events 2 from the .env:\n%s", rec.Code, shown(t, rec))
	}
	form := pageForm(t, rec.Body.String())
	form.Set("v:events", "3")
	form.Set("s:password", savedPassword)
	rec = page(http.MethodPost, "/settings/testmonitor", form, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `hx-get="/settings/testmonitor/progress?shown=restarting"`) {
		t.Fatalf("POST /settings/testmonitor = %d, want 200 with the element that polls the restart:\n%s", rec.Code, shown(t, rec))
	}
	waitPage(t, app, cookie, "testmonitor", textRunning)
	if events, _ := startedEvents(t, b); events != "3" {
		t.Errorf("the monitor started with EVENTS %q, want 3", events)
	}

	rec = page(http.MethodGet, "/settings/testmonitor", nil, false)
	password := regexp.MustCompile(`<input id="f-password"[^>]*>`).FindString(rec.Body.String())
	if !strings.Contains(rec.Body.String(), `<p id="source-password" class="source">Set on this page</p>`) || password == "" || strings.Contains(password, "value=") {
		t.Errorf("the password after the save is %q, want it set on this page and drawn without a value:\n%s", password, shown(t, rec))
	}
	starts := b.starts()
	form = pageForm(t, rec.Body.String())
	form.Set("v:events", "x")
	rec = page(http.MethodPost, "/settings/testmonitor", form, true)
	field := regexp.MustCompile(`(?s)<div class="field has-error">\s*<label for="f-events">.*?</div>`).FindString(rec.Body.String())
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(field, "use a whole number") || !strings.Contains(field, `aria-invalid="true"`) {
		t.Errorf("POST with events x = %d, want 422 with use a whole number next to f-events:\n%s", rec.Code, shown(t, rec))
	}
	form.Set("v:events", "4")
	form.Set("pad", strings.Repeat("x", 140<<10))
	if rec := page(http.MethodPost, "/settings/testmonitor", form, true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a POST of 140 KiB = %d, want 413:\n%s", rec.Code, shown(t, rec))
	}
	if got := b.starts(); !slices.Equal(got, starts) {
		t.Errorf("the monitor started %d times after the refused saves, want none", len(got)-len(starts))
	}

	stop()
	secrets := map[string]string{
		"the saved password":       savedPassword,
		"the password of a person": accountstest.Password,
		"the session cookie":       cookie.Value,
	}
	for name, secret := range secrets {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the logs contain %s", name)
		}
		for i, answer := range answers {
			if strings.Contains(answer.Body.String(), secret) || strings.Contains(fmt.Sprint(answer.Header()), secret) {
				t.Errorf("answer %d contains %s", i, name)
			}
		}
	}
}

// listEdit sends the editor's form with one list operation and returns the editor drawn again.
func listEdit(t *testing.T, app *application, cookie *http.Cookie, form url.Values, op string) string {
	t.Helper()
	form.Set("op", op)
	rec := pageRequest(t, app, http.MethodPost, "/settings/testmonitor/form", form, cookie, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("the list edit %s = %d, want 200:\n%s", op, rec.Code, shown(t, rec))
	}
	return rec.Body.String()
}

// savePage saves the form and waits until the monitor runs with it.
func savePage(t *testing.T, app *application, cookie *http.Cookie, form url.Values) {
	t.Helper()
	if rec := pageRequest(t, app, http.MethodPost, "/settings/testmonitor", form, cookie, true); rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/testmonitor = %d, want 200:\n%s", rec.Code, shown(t, rec))
	}
	waitPage(t, app, cookie, "testmonitor", textRunning)
}

func savedLabels(t *testing.T, app *application, cookie *http.Cookie) any {
	t.Helper()
	document, _ := readSettings(t, app, cookie)["document"].(map[string]any)
	return document["labels"]
}

func TestTheListEditorRoundTrip(t *testing.T) {
	b := newPagesBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	run(t, app)
	waitForMonitor(t, app)
	ensureAdmin(t, app)
	cookie := pageSignIn(t, app, "paulo-santos", accountstest.Password)
	starts := b.starts()
	before := readSettings(t, app, cookie)

	rec := pageRequest(t, app, http.MethodGet, "/settings/testmonitor", nil, cookie, false)
	editor := listEdit(t, app, cookie, pageForm(t, rec.Body.String()), "add:labels")
	form := pageForm(t, editor)
	form.Set("v:labels[0]", "alpha")
	form = pageForm(t, listEdit(t, app, cookie, form, "add:labels"))
	form.Set("v:labels[1]", "beta")
	if got := form["v:labels[0]"]; !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("the second item went in, and the first holds %q, want alpha", got)
	}
	if got := b.starts(); !slices.Equal(got, starts) {
		t.Errorf("the monitor started %d times after the list edits, want none", len(got)-len(starts))
	}
	if after := readSettings(t, app, cookie); !reflect.DeepEqual(after, before) {
		t.Errorf("the list edits changed the configuration: %v, want %v", withoutPasswords(fmt.Sprint(after)), withoutPasswords(fmt.Sprint(before)))
	}

	savePage(t, app, cookie, form)
	if got := savedLabels(t, app, cookie); !reflect.DeepEqual(got, []any{"alpha", "beta"}) {
		t.Errorf("labels after the save = %v, want [alpha beta]", got)
	}

	rec = pageRequest(t, app, http.MethodGet, "/settings/testmonitor", nil, cookie, false)
	editor = listEdit(t, app, cookie, pageForm(t, rec.Body.String()), "remove:labels[0]")
	form = pageForm(t, editor)
	if got := form["v:labels[0]"]; !slices.Equal(got, []string{"beta"}) || form.Has("v:labels[1]") {
		t.Errorf("after removing alpha the items are %q and %q, want only beta", got, form["v:labels[1]"])
	}
	if got := savedLabels(t, app, cookie); !reflect.DeepEqual(got, []any{"alpha", "beta"}) {
		t.Errorf("labels after removing an item without saving = %v, want [alpha beta]", got)
	}

	form.Del("op")
	savePage(t, app, cookie, form)
	if got := savedLabels(t, app, cookie); !reflect.DeepEqual(got, []any{"beta"}) {
		t.Errorf("labels after the second save = %v, want [beta]", got)
	}
}

func TestAnEnvValueThatDoesNotConvertShowsOnThePage(t *testing.T) {
	b := newPagesBench(t, "TESTMONITOR__EVENTS=three")
	app := b.assemble(slog.New(slog.DiscardHandler))
	run(t, app)
	ensureAdmin(t, app)
	cookie := pageSignIn(t, app, "paulo-santos", accountstest.Password)

	rec := pageRequest(t, app, http.MethodGet, "/settings/testmonitor", nil, cookie, false)
	if body := text(t, rec); rec.Code != http.StatusOK || !strings.Contains(body, textEnvBroken) || !strings.Contains(body, `events: EVENTS is not an integer ("three")`) {
		t.Fatalf("GET /settings/testmonitor = %d, want the .env problem of events:\n%s", rec.Code, body)
	}
	form := pageForm(t, rec.Body.String())
	form.Set("v:events", "4")
	if rec := pageRequest(t, app, http.MethodPost, "/settings/testmonitor", form, cookie, true); rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/testmonitor = %d, want 200:\n%s", rec.Code, shown(t, rec))
	}
	waitPage(t, app, cookie, "testmonitor", textRunning)
	if events, _ := startedEvents(t, b); events != "4" {
		t.Errorf("the monitor started with EVENTS %q, want 4", events)
	}
}

func TestTheSettingsPageShowsWhyAMonitorDidNotStart(t *testing.T) {
	b := newPagesBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	run(t, app)
	waitForMonitor(t, app)
	ensureAdmin(t, app)
	cookie := pageSignIn(t, app, "paulo-santos", accountstest.Password)

	rec := pageRequest(t, app, http.MethodGet, "/settings/testmonitor", nil, cookie, false)
	form := pageForm(t, rec.Body.String())
	form["new_name"] = []string{"EXIT_BEFORE_HANDSHAKE", "STDERR_LINES"}
	form["new_value"] = []string{"1", `{"@level":"error","@message":"fatal","error":"connecting to the database: refused"}`}
	form["new_kind"] = []string{"plain", "plain"}
	if rec := pageRequest(t, app, http.MethodPost, "/settings/testmonitor", form, cookie, true); rec.Code != http.StatusOK {
		t.Fatalf("POST with the new variables = %d, want 200:\n%s", rec.Code, shown(t, rec))
	}
	failed := html.UnescapeString(waitPage(t, app, cookie, "testmonitor", "The monitor did not start."))
	if !strings.Contains(failed, "connecting to the database: refused") || strings.Contains(failed, "running with the new values") {
		t.Errorf("the failed start shows %s, want the reason and no running text", withoutPasswords(failed))
	}

	rec = pageRequest(t, app, http.MethodGet, "/settings/testmonitor", nil, cookie, false)
	form = pageForm(t, rec.Body.String())
	form["var_unset"] = []string{"EXIT_BEFORE_HANDSHAKE", "STDERR_LINES"}
	if rec := pageRequest(t, app, http.MethodPost, "/settings/testmonitor", form, cookie, true); rec.Code != http.StatusOK {
		t.Fatalf("POST unsetting the variables = %d, want 200:\n%s", rec.Code, shown(t, rec))
	}
	waitPage(t, app, cookie, "testmonitor", textRunning)
}

func TestThePagesFollowThePermissions(t *testing.T) {
	b := newPagesBench(t)
	app := b.assemble(slog.New(slog.DiscardHandler))
	run(t, app)
	waitForMonitor(t, app)
	ensureAdmin(t, app)
	admin := pageSignIn(t, app, "paulo-santos", accountstest.Password)
	accountstest.Person(t, app.accounts, "testmonitor-viewer", "monitor:testmonitor:view")
	viewer := &http.Cookie{Name: gate.CookieName, Value: accountstest.Session(t, app.accounts, "testmonitor-viewer")}
	starts := b.starts()

	rec := pageRequest(t, app, http.MethodGet, "/settings", nil, viewer, false)
	if body := text(t, rec); rec.Code != http.StatusOK || !strings.Contains(body, "You cannot change the configuration of any monitor. An admin can give you the configure permission.") {
		t.Errorf("GET /settings by a viewer = %d, want 200 saying there is nothing to configure:\n%s", rec.Code, body)
	}
	const noConfigure = "You do not have permission to change this monitor's configuration. An admin can give you the configure permission."
	rec = pageRequest(t, app, http.MethodGet, "/settings/testmonitor", nil, viewer, false)
	if body := text(t, rec); rec.Code != http.StatusForbidden || !strings.Contains(body, noConfigure) {
		t.Errorf("GET /settings/testmonitor by a viewer = %d, want 403 with the refusal:\n%s", rec.Code, body)
	}
	form := pageForm(t, pageRequest(t, app, http.MethodGet, "/settings/testmonitor", nil, admin, false).Body.String())
	form.Set("v:events", "5")
	if rec := pageRequest(t, app, http.MethodPost, "/settings/testmonitor", form, viewer, true); rec.Code != http.StatusForbidden {
		t.Errorf("POST /settings/testmonitor by a viewer = %d, want 403:\n%s", rec.Code, shown(t, rec))
	}
	if got := b.starts(); !slices.Equal(got, starts) {
		t.Errorf("the monitor started %d times after the viewer's save, want none", len(got)-len(starts))
	}

	perms := []access.Permission{{Monitor: "testmonitor", Action: access.View}, {Monitor: "testmonitor", Action: access.Configure}}
	if err := app.accounts.SetPermissions(t.Context(), "testmonitor-viewer", perms); err != nil {
		t.Fatal(err)
	}
	rec = pageRequest(t, app, http.MethodGet, "/settings/testmonitor", nil, viewer, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="f-events"`) || strings.Contains(rec.Body.String(), `action="/settings/testmonitor/check"`) {
		t.Errorf("GET /settings/testmonitor with configure = %d, want 200 with the form and without the check:\n%s", rec.Code, shown(t, rec))
	}

	token := adminToken(t, app)
	rec = request(t, app, http.MethodGet, "/settings", token)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/login" {
		t.Errorf("GET /settings with only a personal token = %d to %q, want 303 to /settings/login", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSigningInAndOutThroughThePages(t *testing.T) {
	b := newPagesBench(t)
	logs := &syncBuffer{}
	app := b.assemble(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	run(t, app)
	waitForMonitor(t, app)
	ensureAdmin(t, app)

	rec := pageRequest(t, app, http.MethodGet, "/settings/login", nil, nil, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<form method="post" action="/settings/login">`) || !strings.Contains(rec.Body.String(), `id="password" name="password" type="password"`) {
		t.Errorf("GET /settings/login = %d, want 200 with the sign-in form:\n%s", rec.Code, shown(t, rec))
	}
	rec = pageRequest(t, app, http.MethodPost, "/settings/login", url.Values{"login": {"paulo-santos"}, "password": {wrongPassword}}, nil, false)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "wrong login or password") || strings.Contains(rec.Body.String(), wrongPassword) {
		t.Errorf("POST /settings/login with a wrong password = %d, want 401 with wrong login or password and without the password:\n%s", rec.Code, shown(t, rec))
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a wrong password set a cookie")
	}

	cookie := pageSignIn(t, app, "paulo-santos", accountstest.Password)
	if rec := pageRequest(t, app, http.MethodGet, "/settings", nil, cookie, false); rec.Code != http.StatusOK {
		t.Errorf("GET /settings after signing in = %d, want 200", rec.Code)
	}
	if rec := sendSigned(t, app, http.MethodGet, "/api/me", cookie, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /api/me with the page's session = %d, want 200:\n%s", rec.Code, shown(t, rec))
	}
	rec = pageRequest(t, app, http.MethodPost, "/settings/logout", nil, cookie, false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/login" {
		t.Errorf("POST /settings/logout = %d to %q, want 303 to /settings/login", rec.Code, rec.Header().Get("Location"))
	}
	rec = pageRequest(t, app, http.MethodGet, "/settings", nil, cookie, false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/login" {
		t.Errorf("GET /settings with the ended session = %d to %q, want 303 to /settings/login", rec.Code, rec.Header().Get("Location"))
	}
	for name, secret := range map[string]string{"the password": accountstest.Password, "the wrong password": wrongPassword} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the logs contain %s", name)
		}
	}
}
