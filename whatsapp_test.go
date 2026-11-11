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

	"github.com/ItzTas/monitor-hub/internal/core"
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

func pairFakeDevice(t *testing.T, databaseURL string) {
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
	device := container.NewDevice()
	device.ID = &types.JID{User: ownerPhone, Device: 7, Server: types.DefaultUserServer}
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := container.PutDevice(t.Context(), device); err != nil {
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

func TestUnpairedWhatsAppWaitsForThePairing(t *testing.T) {
	b := newBench(t, "WHATSAPP_RECIPIENTS="+ownerPhone)
	logs := &syncBuffer{}
	app := b.assemble(slog.New(slog.NewTextHandler(logs, nil)))
	startSupervisor(t, app)
	ctx := t.Context()

	app.ingest.PollOnce(ctx)
	b.setEvents("3")
	app.ingest.PollOnce(ctx)
	app.dispatcher.RunOnce(ctx)

	warning := "WhatsApp is set up but not paired; its notifications wait until you run monitor-hub whatsapp-pair and restart"
	if strings.Count(logs.String(), warning) != 1 || !strings.Contains(logs.String(), "level=WARN") || app.whatsapp == nil || app.whatsapp.session.Paired() {
		t.Errorf("logs:\n%s", logs.String())
	}
	if !slices.Equal(app.channels, []string{"ntfy", "whatsapp"}) || len(b.ntfy.messages()) != 1 {
		t.Errorf("channels %v, ntfy received %+v", app.channels, b.ntfy.messages())
	}
	var waiting, attempts int
	err := app.pool.QueryRow(ctx, "SELECT count(*), coalesce(sum(attempts), 0) FROM deliveries WHERE channel = 'whatsapp' AND status = 'pending'").Scan(&waiting, &attempts)
	if err != nil || waiting != 1 || attempts != 0 {
		t.Errorf("whatsapp deliveries: %d pending with %d attempts (%v)", waiting, attempts, err)
	}
}

func TestWhatsAppThatCannotOpenLeavesTheHubWithoutIt(t *testing.T) {
	pool := coretest.Pool(t)
	pool.Close()
	var logs bytes.Buffer

	link, channels := whatsAppChannels(t.Context(), &core.WhatsApp{Recipients: []string{ownerPhone}}, pool, slog.New(slog.NewTextHandler(&logs, nil)))

	if link != nil || channels != nil || !strings.Contains(logs.String(), `level=ERROR msg="could not open the WhatsApp session; the hub runs without WhatsApp"`) {
		t.Errorf("link %+v, channels %v, logs:\n%s", link, channels, logs.String())
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
