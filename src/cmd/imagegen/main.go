package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/ItzTas/monitor-hub/src/internal/imagegen"
)

const (
	monitorsPath   = "monitors.json"
	dockerfilePath = "Dockerfile"
)

type outOfDate string

func (e outOfDate) Error() string { return string(e) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := run(ctx)
	if err == nil {
		return
	}
	var stale outOfDate
	if errors.As(err, &stale) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "imagegen:", err)
	os.Exit(2)
}

func run(ctx context.Context) error {
	check := flag.Bool("check", false, "fail when the Dockerfile is not what the generator would write")
	flag.Parse()

	entries, err := loadMonitors()
	if err != nil {
		return err
	}
	rendered, err := imagegen.Generate(ctx, entries, &imagegen.DockerReader{})
	if err != nil {
		return err
	}
	if *check {
		return compare(rendered)
	}
	return os.WriteFile(dockerfilePath, rendered, 0o600)
}

func loadMonitors() ([]imagegen.MonitorImage, error) {
	f, err := os.Open(monitorsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := imagegen.LoadMonitors(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", monitorsPath, err)
	}
	return entries, nil
}

func compare(rendered []byte) error {
	current, err := os.ReadFile(dockerfilePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	diff := imagegen.FirstDifference(current, rendered)
	if diff == "" {
		return nil
	}
	return outOfDate(fmt.Sprintf("%s is out of date, run: go run ./cmd/imagegen\nfirst difference at %s", dockerfilePath, diff))
}
