package notify

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)

func TestComposeListsEachTitleWithItsBodyIndented(t *testing.T) {
	events := []Event{
		{Plugin: "sei", DisplayName: "SEI", Kind: "activity", Title: "New activity on process 00000.000001/2026-00", Body: "Process received\nby SEDEMO/NUPROC", OccurredAt: t0},
		{Plugin: "sei", DisplayName: "SEI", Kind: "document", Title: "New document on process 00000.000002/2026-00", URL: "https://sei.example.org/2", OccurredAt: t0},
	}

	msg := Compose("SEI", events)

	want := Message{
		Subject: "2 updates from SEI",
		Body: "New activity on process 00000.000001/2026-00\n" +
			"  Process received\n" +
			"  by SEDEMO/NUPROC\n" +
			"New document on process 00000.000002/2026-00",
		Events: events,
	}
	if !reflect.DeepEqual(msg, want) {
		t.Errorf("message = %+v\nwant      %+v", msg, want)
	}
}

func TestComposeListsTenAndCountsTheRest(t *testing.T) {
	var events []Event
	for i := range 12 {
		events = append(events, Event{Plugin: "diario", Kind: "match", Title: fmt.Sprintf("Edition %02d", i+1), OccurredAt: t0})
	}

	msg := Compose("ALE/RR Official Gazette", events)

	lines := strings.Split(msg.Body, "\n")
	if msg.Subject != "12 updates from ALE/RR Official Gazette" || len(msg.Events) != 12 || len(lines) != 12 ||
		lines[0] != "Edition 01" || lines[9] != "Edition 10" || lines[10] != "" || lines[11] != "and 2 more" {
		t.Errorf("subject %q, %d events, body:\n%s", msg.Subject, len(msg.Events), msg.Body)
	}
}

func TestComposeIsUrgentWhenAnyEventIs(t *testing.T) {
	routine := Event{Plugin: "sei", Kind: "activity", Title: "Activity"}
	urgent := Event{Plugin: "sei", Kind: "return_overdue", Title: "Overdue", Urgent: true}
	cases := map[string]struct {
		events []Event
		want   bool
	}{
		"routine only": {[]Event{routine, routine}, false},
		"one urgent":   {[]Event{routine, urgent}, true},
	}
	for name, tc := range cases {
		if got := Compose("SEI", tc.events).Urgent; got != tc.want {
			t.Errorf("%s: urgent = %v, want %v", name, got, tc.want)
		}
	}
}

func TestTestMessage(t *testing.T) {
	want := Message{
		Subject: "test message",
		Body:    "If you can read this, monitor-hub can reach you on this channel.",
		Events:  []Event{},
		Test:    true,
	}
	if msg := NewTestMessage(); !reflect.DeepEqual(msg, want) {
		t.Errorf("message = %+v", msg)
	}
}
