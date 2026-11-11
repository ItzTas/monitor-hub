package whatsapp

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestLinkProblemsAreLoggedOnceAsErrors(t *testing.T) {
	tests := map[string]struct {
		event any
		want  string
	}{
		"unlinked from the phone": {&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, `level=ERROR msg="the phone unlinked monitor-hub from WhatsApp; WhatsApp notifications wait until you run monitor-hub whatsapp-pair and restart monitor-hub"` + "\n"},
		"replaced by another":     {&events.StreamReplaced{}, `level=ERROR msg="another program connected with this WhatsApp session and disconnected monitor-hub; make sure only one monitor-hub uses this database"` + "\n"},
		"temporarily banned":      {&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour}, `level=ERROR msg="WhatsApp temporarily banned the paired number; WhatsApp notifications fail until the ban ends"` + "\n"},
		"client outdated":         {&events.ClientOutdated{}, `level=ERROR msg="WhatsApp rejected this monitor-hub as outdated; WhatsApp notifications fail until monitor-hub is updated"` + "\n"},
		"anything else":           {&events.PairSuccess{ID: types.NewJID("5595991234567", types.DefaultUserServer)}, ""},
	}
	for name, tt := range tests {
		var out bytes.Buffer
		handle := linkAlerts(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{ReplaceAttr: dropTime})))

		handle(tt.event)

		if out.String() != tt.want {
			t.Errorf("%s: logs %q, want %q", name, out.String(), tt.want)
		}
	}
}
