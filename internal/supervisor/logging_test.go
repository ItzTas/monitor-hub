package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/internal/catalog"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

func stderrLines(lines ...string) string {
	return "TESTPLUGIN__STDERR_LINES=" + strings.Join(lines, "\x1f")
}

func waitForRecords(t *testing.T, buf *logBuffer, want int, match func(map[string]any) bool) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var matched []map[string]any
		for _, record := range buf.records(t) {
			if match(record) {
				matched = append(matched, record)
			}
		}
		if len(matched) >= want || time.Now().After(deadline) {
			return matched
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func messages(records []map[string]any) []string {
	var values []string
	for _, record := range records {
		values = append(values, fmt.Sprint(record["msg"]))
	}
	return values
}

func TestHostileStderrLinesLeaveThePluginRunning(t *testing.T) {
	hostile := []string{
		`{"@message":1}`,
		`{"@level":5,"@message":"x"}`,
		`[]`,
		`null`,
		strings.Repeat("a", 100<<10),
	}
	logger, buf := capture()
	s, _, _ := startLoggingSupervisor(t, []string{stderrLines(append(hostile, `{"@timestamp":7,"@message":"x","@level":"warn"}`)...)}, logger)
	client := waitDescribe(t, s, 10*time.Second)

	raw := waitForRecords(t, buf, len(hostile), func(r map[string]any) bool {
		msg, _ := r["msg"].(string)
		return r["level"] == "DEBUG" && (slices.Contains(hostile, msg) || (r["truncated"] == true && strings.HasPrefix(hostile[4], msg)))
	})
	if got := len(raw); got != len(hostile) {
		t.Errorf("got %d debug records for the %d hostile lines: %v", got, len(hostile), messages(raw))
	}
	warn := waitForRecords(t, buf, 1, func(r map[string]any) bool { return r["level"] == "WARN" && r["msg"] == "x" })
	if len(warn) != 1 {
		t.Errorf("the line with a numeric @timestamp gave %d warn records, want 1", len(warn))
	}
	if !describes(t, client) {
		t.Error("the plugin stopped answering after its stderr lines")
	}
	if started := waitForRecords(t, buf, 0, func(r map[string]any) bool { return r["msg"] == "plugin started" }); len(started) != 1 {
		t.Errorf("the plugin started %d times, want once", len(started))
	}
}

func TestPluginStderrRecordsKeepTheirLevel(t *testing.T) {
	logger, buf := capture()
	s, _, _ := startLoggingSupervisor(t, []string{stderrLines(
		`{"@level":"warn","@message":"careful","@timestamp":"2026-11-10T12:00:00.000Z","answer":42}`,
		`{"@level":"warn","@message":"careful","answer":42}`,
		"plain words on stderr",
	)}, logger)
	waitDescribe(t, s, 10*time.Second)

	got := waitForRecords(t, buf, 3, func(r map[string]any) bool {
		return r["msg"] == "careful" || r["msg"] == "plain words on stderr"
	})

	careful := map[string]any{"level": "WARN", "msg": "careful", "plugin": "testplugin", "answer": json.Number("42")}
	want := []map[string]any{careful, careful, {"level": "DEBUG", "msg": "plain words on stderr", "plugin": "testplugin"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %v\nwant      %v", got, want)
	}
}

func lifecycle(r map[string]any) bool {
	return slices.Contains([]string{"plugin started", "plugin failed to start", "plugin exited", "plugin stopped"}, fmt.Sprint(r["msg"]))
}

func TestPluginLifecycleIsLogged(t *testing.T) {
	logger, buf := capture()
	s, _, stop := startLoggingSupervisor(t, []string{"TESTPLUGIN__CRASH_ON_CHECK=1"}, logger)
	c := waitDescribe(t, s, 10*time.Second)
	if _, err := c.Check(t.Context(), &pluginpb.CheckRequest{}); err == nil {
		t.Fatal("Check succeeded, want the plugin to crash")
	}
	waitForRecords(t, buf, 2, func(r map[string]any) bool { return r["msg"] == "plugin started" })
	stop()

	records := waitForRecords(t, buf, 4, lifecycle)
	if got := messages(records); !slices.Equal(got, []string{"plugin started", "plugin exited", "plugin started", "plugin stopped"}) {
		t.Fatalf("lifecycle records = %v", got)
	}
	for _, record := range records {
		if record["plugin"] != "testplugin" {
			t.Errorf("record without the plugin name: %v", record)
		}
	}
	if exited := records[1]; exited["level"] != "WARN" || exited["restart_in"] == nil || exited["uptime"] == nil {
		t.Errorf("exit record = %v, want a warning with uptime and restart_in", exited)
	}
	for _, record := range buf.records(t) {
		if record["msg"] == "starting plugin" || record["msg"] == "plugin process exited" {
			t.Errorf("go-plugin's own record reached the log: %v", record)
		}
	}
}

func TestPluginThatCannotStartIsLogged(t *testing.T) {
	logger, buf := capture()
	s := New([]catalog.Entry{entry("testplugin", t.TempDir(), "missing-binary")}, nil, t.TempDir(), logger)
	s.restartDelays = []time.Duration{time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	records := waitForRecords(t, buf, 1, lifecycle)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v", err)
	}

	if len(records) != 1 || records[0]["msg"] != "plugin failed to start" || records[0]["level"] != "ERROR" ||
		records[0]["plugin"] != "testplugin" || records[0]["retry_in"] != json.Number(strconv.FormatInt(int64(time.Hour), 10)) || !strings.Contains(fmt.Sprint(records[0]["error"]), "missing-binary") {
		t.Errorf("records = %v, want one error naming the command with retry_in", records)
	}
}
