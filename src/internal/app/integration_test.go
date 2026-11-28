package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/ItzTas/monitor-hub/sdk-go"
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

func TestAVersion1FileIsLoggedAndIgnored(t *testing.T) {
	b := newBench(t)
	installTestmonitor(t, filepath.Join(b.monitorsDir, "testmonitor"), `{"settings_version":1,"fields":[]}`, "")
	logs := &jsonLogs{}
	app := b.assemble(slog.New(slog.NewJSONHandler(logs, nil)))
	run(t, app)
	waitForMonitor(t, app)

	record := logs.waitRecord(t, "the monitor's settings.json is ignored; none of its configuration can be changed here")
	if text, _ := record["error"].(string); record["monitor"] != "testmonitor" || !strings.Contains(text, "settings.json: settings_version is 1, want 2") {
		t.Errorf("the ignored settings.json was logged as %v, want monitor testmonitor and the version error", withoutPasswords(fmt.Sprint(record)))
	}
	if settings := readSettings(t, app, signIn(t, app)); settings["declared"] != false {
		t.Errorf("declared = %v, want false", settings["declared"])
	}
}
