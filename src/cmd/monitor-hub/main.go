package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ItzTas/monitor-hub/src/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := app.Run(ctx, os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
