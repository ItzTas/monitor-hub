package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/ItzTas/monitor-hub/internal/imagegen"
)

const (
	pluginsPath    = "plugins.json"
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

	entries, err := loadPlugins()
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

func loadPlugins() ([]imagegen.PluginImage, error) {
	f, err := os.Open(pluginsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := imagegen.LoadPlugins(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pluginsPath, err)
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
