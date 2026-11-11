package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ItzTas/monitor-hub/internal/catalog"
	"github.com/ItzTas/monitor-hub/sdk"
	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	exitPollInterval = 500 * time.Millisecond
	healthTimeout    = 5 * time.Second
	stopGrace        = 3 * time.Second
)

type Supervisor struct {
	entries       []catalog.Entry
	environ       []string
	dataRoot      string
	log           *slog.Logger
	logOutput     io.Writer
	restartDelays []time.Duration
	stableAfter   time.Duration

	mu      sync.RWMutex
	clients map[string]pluginpb.PluginClient
}

func New(entries []catalog.Entry, environ []string, dataRoot string, log *slog.Logger) *Supervisor {
	return &Supervisor{
		entries:       entries,
		environ:       environ,
		dataRoot:      dataRoot,
		log:           log,
		logOutput:     os.Stdout,
		restartDelays: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute},
		stableAfter:   10 * time.Minute,
		clients:       map[string]pluginpb.PluginClient{},
	}
}

func (s *Supervisor) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, entry := range s.entries {
		wg.Go(func() { s.supervise(ctx, entry) })
	}
	wg.Wait()
	return nil
}

func (s *Supervisor) Client(name string) (pluginpb.PluginClient, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[name]
	return c, ok
}

func (s *Supervisor) Names() []string {
	names := make([]string, 0, len(s.entries))
	for _, entry := range s.entries {
		names = append(names, entry.Manifest.Name)
	}
	slices.Sort(names)
	return names
}

func (s *Supervisor) supervise(ctx context.Context, entry catalog.Entry) {
	failures := 0
	for {
		uptime := s.runOnce(ctx, entry)
		if ctx.Err() != nil {
			return
		}
		if uptime >= s.stableAfter {
			failures = 0
		}
		delay := s.restartDelays[min(failures, len(s.restartDelays)-1)]
		failures++
		s.log.Warn("plugin stopped", "plugin", entry.Manifest.Name, "restart_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (s *Supervisor) runOnce(ctx context.Context, entry catalog.Entry) time.Duration {
	name := entry.Manifest.Name
	var ready atomic.Bool
	client, cmd, err := s.newClient(ctx, entry, &ready)
	if err != nil {
		s.log.Error("plugin failed to start", "plugin", name, "error", err)
		return 0
	}
	conn, rpc, err := dispense(client)
	if err != nil {
		s.log.Error("plugin failed to start", "plugin", name, "error", err)
		stop(client, cmd, false)
		return 0
	}
	ready.Store(true)
	s.setClient(name, rpc)
	s.log.Info("plugin started", "plugin", name)
	started := time.Now()
	waitExit(ctx, client, conn)
	uptime := time.Since(started)
	s.dropClient(name)
	stop(client, cmd, ctx.Err() != nil)
	return uptime
}

func (s *Supervisor) newClient(ctx context.Context, entry catalog.Entry, ready *atomic.Bool) (*plugin.Client, *exec.Cmd, error) {
	env := catalog.Environment(entry.Manifest.Name, s.environ, s.dataRoot)
	if err := os.MkdirAll(envValue(env, sdk.DataDirEnv), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create data dir: %w", err)
	}
	path, err := resolveCommand(entry, env)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, path, entry.Manifest.Command[1:]...)
	cmd.Dir = entry.Dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if ready.Load() {
			return os.ErrProcessDone
		}
		killGroup(cmd)
		return nil
	}
	return plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  sdk.Handshake,
		Plugins:          sdk.PluginSet(),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		SkipHostEnv:      true,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   entry.Manifest.Name,
			Output: s.logOutput,
			Level:  hclog.Debug,
		}),
	}), cmd, nil
}

func dispense(client *plugin.Client) (*grpc.ClientConn, pluginpb.PluginClient, error) {
	rpc, err := client.Client()
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	raw, err := rpc.Dispense(sdk.PluginKey)
	if err != nil {
		return nil, nil, fmt.Errorf("dispense: %w", err)
	}
	return rpc.(*plugin.GRPCClient).Conn, raw.(pluginpb.PluginClient), nil
}

func waitExit(ctx context.Context, client *plugin.Client, conn *grpc.ClientConn) {
	ticker := time.NewTicker(exitPollInterval)
	defer ticker.Stop()
	for !client.Exited() && healthy(ctx, conn) {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func healthy(ctx context.Context, conn *grpc.ClientConn) bool {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: plugin.GRPCServiceName})
	return err == nil
}

func stop(client *plugin.Client, cmd *exec.Cmd, graceful bool) {
	done := make(chan struct{})
	go func() {
		client.Kill()
		close(done)
	}()
	if graceful {
		select {
		case <-done:
		case <-time.After(stopGrace):
		}
	}
	killGroup(cmd)
	<-done
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func (s *Supervisor) setClient(name string, c pluginpb.PluginClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[name] = c
}

func (s *Supervisor) dropClient(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, name)
}

func resolveCommand(entry catalog.Entry, env []string) (string, error) {
	command := entry.Manifest.Command[0]
	local := filepath.Join(entry.Dir, command)
	if filepath.IsLocal(command) && isExecutable(local) {
		return local, nil
	}
	if path, ok := lookPath(command, envValue(env, "PATH")); ok {
		return path, nil
	}
	return "", fmt.Errorf("plugin %s: command %q not found", entry.Manifest.Name, command)
}

func lookPath(command, pathList string) (string, bool) {
	if filepath.IsAbs(command) {
		return command, isExecutable(command)
	}
	if strings.ContainsRune(command, filepath.Separator) {
		return "", false
	}
	for _, dir := range filepath.SplitList(pathList) {
		candidate := filepath.Join(dir, command)
		if filepath.IsAbs(dir) && isExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
