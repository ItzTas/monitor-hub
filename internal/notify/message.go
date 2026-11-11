package notify

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const maxListed = 10

type Event struct {
	Plugin      string    `json:"plugin"`
	DisplayName string    `json:"display_name"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Body        string    `json:"body,omitempty"`
	URL         string    `json:"url,omitempty"`
	Urgent      bool      `json:"urgent"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type Message struct {
	Subject string
	Body    string
	Events  []Event
	Urgent  bool
	Test    bool
}

func Compose(displayName string, events []Event) Message {
	msg := Message{Subject: subject(displayName, len(events)), Events: events}
	items := make([]string, 0, min(len(events), maxListed))
	for _, e := range events {
		msg.Urgent = msg.Urgent || e.Urgent
		if len(items) < maxListed {
			items = append(items, item(e))
		}
	}
	msg.Body = strings.Join(items, "\n")
	if hidden := len(events) - len(items); hidden > 0 {
		msg.Body += fmt.Sprintf("\n\nand %d more", hidden)
	}
	return msg
}

func NewTestMessage() Message {
	return Message{
		Subject: "test message",
		Body:    "If you can read this, monitor-hub can reach you on this channel.",
		Events:  []Event{},
		Test:    true,
	}
}

func item(e Event) string {
	if e.Body == "" {
		return e.Title
	}
	return e.Title + "\n  " + strings.ReplaceAll(e.Body, "\n", "\n  ")
}

func subject(displayName string, count int) string {
	if count == 1 {
		return "1 update from " + displayName
	}
	return fmt.Sprintf("%d updates from %s", count, displayName)
}

func cut(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const marker = "\n…"
	end := limit - len(marker)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + marker
}
