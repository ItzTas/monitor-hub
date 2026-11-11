package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/internal/api"
	"github.com/ItzTas/monitor-hub/internal/catalog"
	"github.com/ItzTas/monitor-hub/internal/core"
	"github.com/ItzTas/monitor-hub/internal/ingest"
	"github.com/ItzTas/monitor-hub/internal/modules/feed"
	"github.com/ItzTas/monitor-hub/internal/modules/health"
	"github.com/ItzTas/monitor-hub/internal/notify"
	"github.com/ItzTas/monitor-hub/internal/supervisor"
)

type application struct {
	address    string
	log        *slog.Logger
	pool       *pgxpool.Pool
	supervisor *supervisor.Supervisor
	ingest     *ingest.Service
	dispatcher *notify.Dispatcher
	channels   []string
	handler    http.Handler
}

func assemble(ctx context.Context, cfg core.Config, environ []string, logger *slog.Logger, now func() time.Time) (*application, error) {
	entries, err := catalog.Load(cfg.PluginsDir)
	if err != nil {
		return nil, err
	}
	pool, err := core.OpenDatabase(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	plugins := supervisor.New(entries, environ, cfg.DataDir, logger)
	channels := notify.Channels(cfg.Channels, userAgent())
	names := notify.ChannelNames(channels)
	mux := http.NewServeMux()
	health.Configure(mux, pool, logger)
	feed.Configure(mux, pool, feed.Options{Token: cfg.FeedToken, PublicURL: cfg.PublicURL}, logger)
	api.Register(mux, api.Deps{Pool: pool, Plugins: plugins, Token: cfg.APIToken, Log: logger})
	return &application{
		address:    cfg.HTTPAddress,
		log:        logger,
		pool:       pool,
		supervisor: plugins,
		ingest:     ingest.NewService(ingest.NewPostgres(pool), plugins, names, logger, now),
		dispatcher: notify.NewDispatcher(notify.NewOutbox(pool), channels, logger, notify.WithClock(now, time.After)),
		channels:   names,
		handler:    withMiddleware(mux, logger),
	}, nil
}

func (a *application) Run(ctx context.Context) error {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", a.address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.address, err)
	}
	a.log.Info("monitor-hub starting",
		"version", version,
		"address", listener.Addr().String(),
		"plugins", strings.Join(a.supervisor.Names(), ","),
		"channels", strings.Join(a.channels, ","),
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.supervisor.Run(gctx) })
	g.Go(func() error { return a.ingest.Run(gctx) })
	g.Go(func() error { return a.dispatcher.Run(gctx) })
	g.Go(func() error { return serveHTTP(gctx, newServer(a.handler, a.log), listener) })
	return g.Wait()
}

func (a *application) Close() {
	a.pool.Close()
}
