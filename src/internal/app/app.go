package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api"
	"github.com/ItzTas/monitor-hub/src/internal/api/session"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/consumerapi"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/core"
	"github.com/ItzTas/monitor-hub/src/internal/icons"
	"github.com/ItzTas/monitor-hub/src/internal/ingest"
	"github.com/ItzTas/monitor-hub/src/internal/modules/feed"
	"github.com/ItzTas/monitor-hub/src/internal/modules/health"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
	"github.com/ItzTas/monitor-hub/src/internal/notify"
	"github.com/ItzTas/monitor-hub/src/internal/stream"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

type application struct {
	address    string
	log        *slog.Logger
	pool       *pgxpool.Pool
	supervisor *supervisor.Supervisor
	ingest     *ingest.Service
	dispatcher *notify.Dispatcher
	channels   []string
	closers    []io.Closer
	handler    http.Handler
	whatsapp   *whatsAppLink
	signal     *stream.Signal
	accounts   *accounts.Service
}

func assemble(ctx context.Context, cfg core.Config, environ []string, logger *slog.Logger, now func() time.Time) (*application, error) {
	entries, err := catalog.Load(cfg.MonitorsDir)
	if err != nil {
		return nil, err
	}
	warnIgnoredSettings(entries, logger)
	pool, err := core.OpenDatabase(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	store := monitorconfig.NewPostgres(pool)
	documents := monitorconfig.NewDocuments(entries, environ, cfg.DataDir, store, logger)
	monitors := supervisor.New(entries, environ, cfg.DataDir, logger, supervisor.WithDocuments(documents))
	accountService := accounts.NewService(pool, now)
	if err := accountService.Prepare(ctx, monitors.Names()); err != nil {
		pool.Close()
		return nil, err
	}
	settings := monitorconfig.NewService(monitorconfig.Deps{
		Entries:    entries,
		Environ:    environ,
		DataRoot:   cfg.DataDir,
		Store:      store,
		Supervisor: monitors,
		Checker:    monitorconfig.NewValidator(),
		Log:        logger,
		Now:        now,
	})
	settings.WarnIgnored(ctx)
	link, sessions := whatsAppChannels(ctx, cfg.Channels.WhatsApp, pool, logger)
	channels := notify.Channels(cfg.Channels, userAgent(), sessions...)
	names := notify.ChannelNames(channels)
	consumerService := consumers.NewService(pool)
	signal := stream.NewSignal()
	registry := stream.NewRegistry()
	mux := http.NewServeMux()
	health.Configure(mux, pool, logger)
	feed.Configure(mux, pool, feed.Options{PublicURL: cfg.PublicURL}, accountService, logger)
	icons.Register(mux)
	consumerapi.Register(mux, consumerapi.Deps{Consumers: consumerService, Stream: stream.NewService(stream.NewPostgres(pool), signal, registry), Log: logger})
	api.Register(mux, api.Deps{Pool: pool, Monitors: monitors, Accounts: accountService, PublicURL: cfg.PublicURL, Log: logger, Consumers: consumerService, Registry: registry, Settings: settings})
	logins := session.NewLogins(time.After, nil)
	api.RegisterAccess(mux, api.AccessDeps{Accounts: accountService, Log: logger, PublicURL: cfg.PublicURL, Logins: logins})
	dispatcher := notify.NewDispatcher(notify.NewOutbox(pool), channels, logger, notify.WithClock(now, time.After))
	return &application{
		address:    cfg.HTTPAddress,
		log:        logger,
		pool:       pool,
		supervisor: monitors,
		ingest:     ingest.NewService(ingest.NewPostgres(pool), monitors, names, logger, now, ingest.WithWake(dispatcher.Wake), ingest.WithNewEvents(signal.Notify)),
		dispatcher: dispatcher,
		channels:   names,
		closers:    closers(channels),
		handler:    withMiddleware(mux, logger),
		whatsapp:   link,
		signal:     signal,
		accounts:   accountService,
	}, nil
}

func warnIgnoredSettings(entries []catalog.Entry, logger *slog.Logger) {
	for _, entry := range entries {
		if entry.SettingsError != nil {
			logger.Warn("the monitor's settings.json is ignored; none of its configuration can be changed here", "monitor", entry.Manifest.Name, "error", entry.SettingsError)
		}
	}
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
		"monitors", strings.Join(a.supervisor.Names(), ","),
		"channels", strings.Join(a.channels, ","),
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.supervisor.Run(gctx) })
	g.Go(func() error { return a.ingest.Run(gctx) })
	g.Go(func() error { return a.dispatcher.Run(gctx) })
	server := newServer(a.handler, a.log)
	server.RegisterOnShutdown(a.signal.Close)
	g.Go(func() error { return serveHTTP(gctx, server, listener) })
	return g.Wait()
}

func closers(channels []notify.Channel) []io.Closer {
	var found []io.Closer
	for _, channel := range channels {
		if closer, ok := channel.(io.Closer); ok {
			found = append(found, closer)
		}
	}
	return found
}

func (a *application) Close() {
	a.whatsapp.Close()
	for _, closer := range a.closers {
		if err := closer.Close(); err != nil {
			a.log.Warn("could not close a notification channel", "error", err)
		}
	}
	a.pool.Close()
}
