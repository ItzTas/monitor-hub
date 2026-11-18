package whatsapp

import (
	"log/slog"
	"reflect"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

func linkAlerts(log *slog.Logger) whatsmeow.EventHandler {
	messages := map[reflect.Type]string{
		reflect.TypeFor[*events.LoggedOut]():      "the phone unlinked monitor-hub from WhatsApp; WhatsApp notifications wait until you run monitor-hub whatsapp-pair and restart monitor-hub",
		reflect.TypeFor[*events.StreamReplaced](): "another program connected with this WhatsApp session and disconnected monitor-hub; make sure only one monitor-hub uses this database",
		reflect.TypeFor[*events.TemporaryBan]():   "WhatsApp temporarily banned the paired number; WhatsApp notifications fail until the ban ends",
		reflect.TypeFor[*events.ClientOutdated](): "WhatsApp rejected this monitor-hub as outdated; WhatsApp notifications fail until monitor-hub is updated",
	}
	return func(evt any) {
		if message, ok := messages[reflect.TypeOf(evt)]; ok {
			log.Error(message)
		}
	}
}
