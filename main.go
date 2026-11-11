package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ItzTas/monitor-hub/internal/core"
)

var version = "dev"

type terminal struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

type usageError struct {
	error
}

type command func(ctx context.Context, args, environ []string, term terminal) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	commands := map[string]command{
		"serve":         serve,
		"whatsapp-pair": whatsAppPair,
	}
	name := "serve"
	if len(args) > 0 {
		name, args = args[0], args[1:]
	}
	selected, ok := commands[name]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "unknown command %q; use one of: %s\n", name, strings.Join(slices.Sorted(maps.Keys(commands)), ", "))
		return 2
	}
	err := selected(ctx, args, environ, terminal{stdin: stdin, stdout: stdout, stderr: stderr})
	if usage, ok := errors.AsType[usageError](err); ok {
		_, _ = fmt.Fprintln(stderr, usage.Error())
		return 2
	}
	if err != nil {
		slog.New(slog.NewTextHandler(stdout, nil)).Error("fatal", "err", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, args, environ []string, term terminal) error {
	if len(args) > 0 {
		return usageError{errors.New("serve takes no arguments")}
	}
	cfg, err := core.Load(environ, os.ReadFile)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(term.stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	app, err := assemble(ctx, cfg, environ, logger, time.Now)
	if err != nil {
		return err
	}
	defer app.Close()
	return app.Run(ctx)
}

func userAgent() string {
	return "monitor-hub/" + version
}
