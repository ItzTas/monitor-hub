package main

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
)

const ownerPhone = "5595991234567"

func (b *bench) value(key string) string {
	for _, entry := range b.environ {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value
		}
	}
	return ""
}

func sessionStore(t *testing.T, databaseURL string) (*pgxpool.Pool, *sqlstore.Container) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	container := sqlstore.NewWithDB(db, "postgres", waLog.Noop)
	if err := container.Upgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool, container
}

func pairFakeDevice(t *testing.T, databaseURL string) {
	t.Helper()
	_, container := sessionStore(t, databaseURL)
	device := container.NewDevice()
	device.ID = &types.JID{User: ownerPhone, Device: 7, Server: types.DefaultUserServer}
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := container.PutDevice(t.Context(), device); err != nil {
		t.Fatal(err)
	}
}

func storeFromANewerHub(t *testing.T, databaseURL string) {
	t.Helper()
	pool, _ := sessionStore(t, databaseURL)
	if _, err := pool.Exec(t.Context(), "UPDATE whatsmeow_version SET version = 999, compat = 999"); err != nil {
		t.Fatal(err)
	}
}

func TestWhatsAppChannelRegisteredWhenConfigured(t *testing.T) {
	b := newBench(t, "WHATSAPP_RECIPIENTS="+ownerPhone)
	pairFakeDevice(t, b.value("DATABASE_URL"))
	logs := &syncBuffer{}

	app := b.assemble(slog.New(slog.NewTextHandler(logs, nil)))

	if !slices.Equal(app.channels, []string{"ntfy", "whatsapp"}) || app.whatsapp == nil || !app.whatsapp.session.Paired() || strings.Contains(logs.String(), "WhatsApp") {
		t.Errorf("channels %v, session %+v, logs:\n%s", app.channels, app.whatsapp, logs.String())
	}
}

func linesWith(text, part string) int {
	count := 0
	for line := range strings.Lines(text) {
		if strings.Contains(line, part) {
			count++
		}
	}
	return count
}

func TestWhatsAppDeliveriesWaitWhileItCannotSend(t *testing.T) {
	tests := map[string]struct {
		prepare func(t *testing.T, databaseURL string)
		log     string
		session bool
	}{
		"not paired": {
			prepare: func(*testing.T, string) {},
			log:     `level=WARN msg="WhatsApp is set up but not paired; its notifications wait until you run monitor-hub whatsapp-pair and restart"`,
			session: true,
		},
		"session store cannot open": {
			prepare: storeFromANewerHub,
			log:     `level=ERROR msg="could not open the WhatsApp session; WhatsApp stays disabled until monitor-hub restarts" error="whatsapp: prepare the session tables: unsupported database schema version: currently on v999`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b := newBench(t, "WHATSAPP_RECIPIENTS="+ownerPhone)
			tt.prepare(t, b.value("DATABASE_URL"))
			logs := &syncBuffer{}
			app := b.assemble(slog.New(slog.NewTextHandler(logs, nil)))
			startSupervisor(t, app)
			ctx := t.Context()

			app.ingest.PollOnce(ctx)
			b.setEvents("3")
			app.ingest.PollOnce(ctx)
			app.dispatcher.RunOnce(ctx)

			if strings.Count(logs.String(), tt.log) != 1 || linesWith(logs.String(), "WhatsApp") != 1 || (app.whatsapp != nil) != tt.session {
				t.Errorf("session open %v, logs:\n%s", app.whatsapp != nil, logs.String())
			}
			if !slices.Equal(app.channels, []string{"ntfy", "whatsapp"}) || len(b.ntfy.messages()) != 1 {
				t.Errorf("channels %v, ntfy received %+v", app.channels, b.ntfy.messages())
			}
			var waiting, attempts int
			err := app.pool.QueryRow(ctx, "SELECT count(*), coalesce(sum(attempts), 0) FROM deliveries WHERE channel = 'whatsapp' AND status = 'pending'").Scan(&waiting, &attempts)
			if err != nil || waiting != 1 || attempts != 0 {
				t.Errorf("whatsapp deliveries: %d pending with %d attempts (%v)", waiting, attempts, err)
			}
		})
	}
}

func TestWhatsAppPairRefusesABadPhoneNumber(t *testing.T) {
	environ := []string{"DATABASE_URL=" + coretest.Database(t)}
	tests := map[string][]string{
		"without the area code": {"whatsapp-pair", "991234567"},
		"with a leading zero":   {"whatsapp-pair", "095991234567"},
		"two numbers":           {"whatsapp-pair", ownerPhone, "5595991234568"},
	}
	for name, args := range tests {
		var stdout, stderr bytes.Buffer

		code := run(t.Context(), args, environ, strings.NewReader(""), &stdout, &stderr)

		if code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%s: exit code %d, stdout %q, stderr %q", name, code, stdout.String(), stderr.String())
		}
	}
}

func TestWhatsAppPairNeedsOnlyTheDatabase(t *testing.T) {
	databaseURL := coretest.Database(t)
	pairFakeDevice(t, databaseURL)
	var stdout, stderr bytes.Buffer

	code := run(t.Context(), []string{"whatsapp-pair"}, []string{"DATABASE_URL=" + databaseURL}, strings.NewReader("+55 (95) 99123-4567\n"), &stdout, &stderr)

	prompt := "Phone number of the WhatsApp account that will send the messages, with country and area code (e.g. 5595991234567): "
	refusal := "this database already has a paired WhatsApp number. To pair another one, unlink monitor-hub under Linked devices on the paired phone, start monitor-hub once so it drops the old session, and run whatsapp-pair again"
	if code != 1 || !strings.HasPrefix(stdout.String(), prompt) || !strings.Contains(stdout.String(), refusal) || stderr.Len() != 0 {
		t.Errorf("exit code %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}
