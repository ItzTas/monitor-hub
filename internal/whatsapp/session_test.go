package whatsapp_test

import (
	"database/sql"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
	"github.com/ItzTas/monitor-hub/internal/whatsapp"
)

const owner = "5595991234567"

func openDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := stdlib.OpenDBFromPool(coretest.Pool(t))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openSession(t *testing.T, db *sql.DB) *whatsapp.Session {
	t.Helper()
	session, err := whatsapp.Open(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	return session
}

func TestNewSessionIsNotPairedAndDoesNotConnect(t *testing.T) {
	session := openSession(t, openDatabase(t))

	err := session.ConnectContext(t.Context())

	if session.Paired() || !errors.Is(err, whatsapp.ErrNotPaired) || session.IsConnected() {
		t.Errorf("paired %v, connect = %v, connected %v", session.Paired(), err, session.IsConnected())
	}
}

func TestPairedSessionLivesInTheDatabaseAndRefusesAnotherPairing(t *testing.T) {
	db := openDatabase(t)
	openSession(t, db)
	container := sqlstore.NewWithDB(db, "postgres", waLog.Noop)
	device := container.NewDevice()
	device.ID = &types.JID{User: owner, Device: 7, Server: types.DefaultUserServer}
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := container.PutDevice(t.Context(), device); err != nil {
		t.Fatal(err)
	}

	session := openSession(t, db)
	err := session.Pair(t.Context(), owner, func(string) { t.Error("showed a pairing code") })

	if !session.Paired() || !errors.Is(err, whatsapp.ErrAlreadyPaired) {
		t.Errorf("paired %v, Pair = %v", session.Paired(), err)
	}
}
