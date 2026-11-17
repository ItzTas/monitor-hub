package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ItzTas/monitor-hub/sdk/plugintest"
)

const (
	pluginPrefix   = "plugin: "
	exitOK         = 0
	exitViolations = 1
	exitUsage      = 2
	usage          = "usage: plugin-conformance [-min-events N] [-ready-timeout D] [-plugin-dir DIR] [-env NAME=value]... -- <command> [args...]"
)

type envFlags []string

type prefixed struct {
	out  io.Writer
	line []byte
}

func (p *prefixed) Write(b []byte) (int, error) {
	p.line = append(p.line, b...)
	for {
		end := bytes.IndexByte(p.line, '\n')
		if end < 0 {
			return len(b), nil
		}
		if _, err := p.out.Write(append([]byte(pluginPrefix), p.line[:end+1]...)); err != nil {
			return len(b), err
		}
		p.line = p.line[end+1:]
	}
}

func (e *envFlags) String() string { return strings.Join(*e, ",") }

func (e *envFlags) Set(v string) error {
	if name, _, ok := strings.Cut(v, "="); !ok || name == "" {
		return fmt.Errorf("%q is not NAME=value", v)
	}
	*e = append(*e, v)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var (
		env       envFlags
		opts      plugintest.Options
		pluginDir string
	)
	fs := flag.NewFlagSet("plugin-conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&opts.MinEvents, "min-events", 0, "minimum number of events the plugin must return")
	fs.DurationVar(&opts.ReadyTimeout, "ready-timeout", 0, "how long to wait while Events answers UNAVAILABLE")
	fs.StringVar(&pluginDir, "plugin-dir", "", "plugin directory: check its settings.json and run <command> validate-settings in it")
	fs.Var(&env, "env", "environment variable NAME=value for the plugin (repeatable)")

	sep := slices.Index(args, "--")
	if sep < 0 || sep == len(args)-1 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if err := fs.Parse(args[:sep]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	dir, err := settingsDir(pluginDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "plugin-conformance: -plugin-dir: %v\n", err)
		return exitUsage
	}

	command := args[sep+1:]
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = env
	settings := checkSettings(ctx, dir, cmd.Path, stderr)
	opts.Stderr = &prefixed{out: stderr}
	violations, err := plugintest.Check(ctx, cmd, opts)
	writeLines(stderr, settings)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "plugin-conformance: %v\n", oneLine(err))
		return exitViolations
	}
	writeLines(stderr, violations)
	if len(settings)+len(violations) > 0 {
		return exitViolations
	}
	_, _ = fmt.Fprintf(stdout, "conformance passed (at least %d events required, ready timeout %s)\n", opts.MinEvents, opts.ReadyTimeout.Round(time.Millisecond))
	return exitOK
}

func settingsDir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return filepath.Abs(dir)
}

func checkSettings(ctx context.Context, dir, program string, stderr io.Writer) []string {
	if dir == "" {
		return nil
	}
	return plugintest.CheckSettings(ctx, dir, program, &prefixed{out: stderr})
}

func writeLines(w io.Writer, lines []string) {
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
