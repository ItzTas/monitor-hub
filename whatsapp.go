package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ItzTas/monitor-hub/internal/core"
	"github.com/ItzTas/monitor-hub/internal/notify"
	"github.com/ItzTas/monitor-hub/internal/whatsapp"
)

const pairingTimeout = 5 * time.Minute

type whatsAppLink struct {
	db      *sql.DB
	session *whatsapp.Session
}

type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

func openWhatsApp(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (*whatsAppLink, error) {
	db := stdlib.OpenDBFromPool(pool)
	session, err := whatsapp.Open(ctx, db, logger)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &whatsAppLink{db: db, session: session}, nil
}

func (l *whatsAppLink) Close() {
	if l == nil {
		return
	}
	l.session.Close()
	_ = l.db.Close()
}

func whatsAppChannels(ctx context.Context, cfg *core.WhatsApp, pool *pgxpool.Pool, logger *slog.Logger) (*whatsAppLink, []notify.Channel) {
	if cfg == nil {
		return nil, nil
	}
	link, err := openWhatsApp(ctx, pool, logger)
	if err != nil {
		logger.Error("could not open the WhatsApp session; the hub runs without WhatsApp", "error", err)
		return nil, nil
	}
	if !link.session.Paired() {
		logger.Warn("WhatsApp is set up but not paired; its notifications wait until you run monitor-hub whatsapp-pair and restart")
	}
	return link, []notify.Channel{whatsapp.NewChannel(link.session, cfg.Recipients)}
}

func whatsAppPair(ctx context.Context, args, environ []string, term terminal) error {
	if len(args) > 1 {
		return usageError{errors.New("whatsapp-pair takes at most one argument, the phone number")}
	}
	phone, err := pairingPhone(args, term)
	if err != nil {
		return err
	}
	databaseURL, err := core.LoadDatabaseURL(environ, os.ReadFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, pairingTimeout)
	defer cancel()
	pool, err := core.OpenDatabase(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	link, err := openWhatsApp(ctx, pool, slog.New(slog.NewTextHandler(term.stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		return err
	}
	defer link.Close()
	out := &printer{w: term.stdout}
	err = link.session.Pair(ctx, phone, func(code string) {
		out.printf("Pairing code: %s\n\n", code)
		out.printf("On the phone with number %s, open WhatsApp > Linked devices > Link a device, tap \"Link with phone number instead\" and type the code. It expires in a few minutes.\n", phone)
	})
	if errors.Is(err, whatsapp.ErrAlreadyPaired) {
		return errors.New("this database already has a paired WhatsApp number. To pair another one, unlink monitor-hub under Linked devices on the paired phone, start monitor-hub once so it drops the old session, and run whatsapp-pair again")
	}
	if err != nil {
		return err
	}
	out.printf("Paired. Restart monitor-hub so it starts sending WhatsApp messages.\n")
	return out.err
}

func pairingPhone(args []string, term terminal) (string, error) {
	answer, err := askPhone(args, term)
	if err != nil {
		return "", err
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, answer)
	if len(digits) < 10 || len(digits) > 15 || strings.HasPrefix(digits, "0") {
		return "", usageError{errors.New("the phone number needs the country and area code, digits only (e.g. 5595991234567)")}
	}
	return digits, nil
}

func askPhone(args []string, term terminal) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	out := &printer{w: term.stdout}
	out.printf("Phone number of the WhatsApp account that will send the messages, with country and area code (e.g. 5595991234567): ")
	line, err := bufio.NewReader(term.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, out.err
}
