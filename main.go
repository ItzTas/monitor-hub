package main

import (
	"context"
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

type command func(ctx context.Context, environ []string, stdout io.Writer) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args, environ []string, stdout, stderr io.Writer) int {
	commands := map[string]command{
		"serve": serve,
	}
	name := "serve"
	if len(args) > 0 {
		name = args[0]
	}
	selected, ok := commands[name]
	if !ok || len(args) > 1 {
		_, _ = fmt.Fprintf(stderr, "unknown command %q; use one of: %s\n", strings.Join(args, " "), strings.Join(slices.Sorted(maps.Keys(commands)), ", "))
		return 2
	}
	if err := selected(ctx, environ, stdout); err != nil {
		slog.New(slog.NewTextHandler(stdout, nil)).Error("fatal", "err", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, environ []string, stdout io.Writer) error {
	cfg, err := core.Load(environ, os.ReadFile)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
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
