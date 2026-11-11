package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ItzTas/monitor-hub/sdk/plugintest"
)

const (
	exitOK         = 0
	exitViolations = 1
	exitUsage      = 2
	usage          = "usage: plugin-conformance [-min-events N] [-ready-timeout D] [-env NAME=value]... -- <command> [args...]"
)

type envFlags []string

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
		env  envFlags
		opts plugintest.Options
	)
	fs := flag.NewFlagSet("plugin-conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&opts.MinEvents, "min-events", 0, "minimum number of events the plugin must return")
	fs.DurationVar(&opts.ReadyTimeout, "ready-timeout", 0, "how long to wait while Events answers UNAVAILABLE")
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

	command := args[sep+1:]
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = env
	violations, err := plugintest.Check(ctx, cmd, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "plugin-conformance: %v\n", oneLine(err))
		return exitViolations
	}
	if len(violations) > 0 {
		for _, v := range violations {
			_, _ = fmt.Fprintln(stderr, v)
		}
		return exitViolations
	}
	_, _ = fmt.Fprintf(stdout, "conformance passed (at least %d events required, ready timeout %s)\n", opts.MinEvents, opts.ReadyTimeout.Round(time.Millisecond))
	return exitOK
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
