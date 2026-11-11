package examples

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"github.com/ItzTas/monitor-hub/sdk/plugintest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	pythonEnv   = "MONITOR_HUB_EXAMPLE_PYTHON"
	magicCookie = "MONITOR_HUB_PLUGIN=7c0e5a52-monitor-hub"
)

func pythonInterpreter(t *testing.T) string {
	t.Helper()
	name := os.Getenv(pythonEnv)
	if name == "" {
		name = "python3"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		skipOrFail(t, "%s is not on PATH", name)
	}
	probe := exec.CommandContext(t.Context(), path, "-c", "import grpc, google.protobuf")
	if out, err := probe.CombinedOutput(); err != nil {
		skipOrFail(t, "%s cannot import grpc and protobuf: %s", path, out)
	}
	return path
}

func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format+" (required in CI)", args...)
	}
	t.Skipf(format, args...)
}

func pythonScript(t *testing.T) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "python-plugin", "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func TestPythonPluginConformance(t *testing.T) {
	python := pythonInterpreter(t)
	script := pythonScript(t)
	dataDir := t.TempDir()
	socketDir, err := os.MkdirTemp("", "py")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })

	cmd := exec.CommandContext(t.Context(), python, script)
	cmd.Dir = filepath.Dir(script)
	cmd.Env = []string{magicCookie, "MONITOR_HUB_DATA_DIR=" + dataDir, "TMPDIR=" + socketDir}

	violations, err := plugintest.Check(t.Context(), cmd, plugintest.Options{MinEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Errorf("conformance: %s", violation)
	}

	assertNoProcess(t, script)
	entries, err := os.ReadDir(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("socket dir not empty after shutdown: %v", entries)
	}
}

func TestPythonPluginRefusesToRunByHand(t *testing.T) {
	python := pythonInterpreter(t)
	script := pythonScript(t)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), python, script)
	cmd.Env = []string{"MONITOR_HUB_DATA_DIR=" + t.TempDir(), "TMPDIR=" + t.TempDir()}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v, want exit status 1", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Error("stderr is empty, want a message")
	}
}

type pythonPlugin struct {
	cmd       *exec.Cmd
	conn      *grpc.ClientConn
	socket    string
	socketDir string
}

func startPython(t *testing.T, dataDir string) *pythonPlugin {
	t.Helper()
	python := pythonInterpreter(t)
	script := pythonScript(t)
	socketDir, err := os.MkdirTemp("", "py")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })

	cmd := exec.CommandContext(t.Context(), python, script)
	cmd.Env = []string{magicCookie, "MONITOR_HUB_DATA_DIR=" + dataDir, "TMPDIR=" + socketDir}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("read handshake: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(line), "|")
	if len(fields) != 5 || fields[2] != "unix" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("handshake = %q", line)
	}
	conn, err := grpc.NewClient("unix:"+fields[3], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	p := &pythonPlugin{cmd: cmd, conn: conn, socket: fields[3], socketDir: socketDir}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return p
}

func (p *pythonPlugin) shutdown(t *testing.T) {
	t.Helper()
	err := p.conn.Invoke(t.Context(), "/plugin.GRPCController/Shutdown", &emptypb.Empty{}, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("plugin exit: %v", err)
	}
	if _, err := os.Stat(p.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket %s still exists after shutdown (stat error: %v)", p.socket, err)
	}
}

func (p *pythonPlugin) events(t *testing.T) []*pluginpb.Event {
	t.Helper()
	resp, err := pluginpb.NewPluginClient(p.conn).Events(t.Context(), &pluginpb.EventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetEvents()
}

func (p *pythonPlugin) check(t *testing.T) {
	t.Helper()
	resp, err := pluginpb.NewPluginClient(p.conn).Check(t.Context(), &pluginpb.CheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("Check refused: %s", resp.GetMessage())
	}
}

func titles(events []*pluginpb.Event) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.GetTitle())
	}
	return out
}

func TestPythonPluginHealthAndShutdown(t *testing.T) {
	p := startPython(t, t.TempDir())
	health := grpc_health_v1.NewHealthClient(p.conn)

	resp, err := health.Check(t.Context(), &grpc_health_v1.HealthCheckRequest{Service: "plugin"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.GetStatus())
	}

	_, err = health.Check(t.Context(), &grpc_health_v1.HealthCheckRequest{Service: "other"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown service: code = %v, want NotFound", status.Code(err))
	}

	p.shutdown(t)
}

func deleteAllEvents(t *testing.T, dataDir string) {
	t.Helper()
	database := filepath.Join(dataDir, "events.sqlite3")
	if _, err := os.Stat(database); err != nil {
		t.Fatalf("plugin database: %v", err)
	}
	script := "import sqlite3, sys\nconn = sqlite3.connect(sys.argv[1])\nconn.execute('DELETE FROM events')\nconn.commit()\nconn.close()"
	out, err := exec.CommandContext(t.Context(), pythonInterpreter(t), "-I", "-c", script, database).CombinedOutput()
	if err != nil {
		t.Fatalf("delete events: %v\n%s", err, out)
	}
}

func TestPythonPluginNeverReusesSeqAfterEventsAreDeleted(t *testing.T) {
	dataDir := t.TempDir()

	first := startPython(t, dataDir)
	for range 3 {
		first.check(t)
	}
	var largest int64
	for _, event := range first.events(t) {
		largest = max(largest, event.GetSeq())
	}
	first.shutdown(t)

	deleteAllEvents(t, dataDir)

	second := startPython(t, dataDir)
	if got := second.events(t); len(got) != 0 {
		t.Fatalf("events after the delete = %v, want none (the first-run event is created once)", titles(got))
	}
	second.check(t)
	got := second.events(t)
	second.shutdown(t)

	if len(got) != 1 {
		t.Fatalf("events = %v, want exactly one", titles(got))
	}
	if got[0].GetSeq() <= largest {
		t.Errorf("seq = %d, want more than %d", got[0].GetSeq(), largest)
	}
}
