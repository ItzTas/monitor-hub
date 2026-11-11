package events

import (
	"encoding/json"
	"time"
)

type ListInput struct {
	Plugin string `query:"plugin" doc:"Only the events of this plugin."`
	Kind   string `query:"kind" doc:"Only the events of this kind."`
	Before int64  `query:"before" minimum:"1" doc:"Only the events whose seq is lower than this; pass the next_before of the previous page."`
	Limit  int    `query:"limit" default:"50" minimum:"1" doc:"Events per page. Values over 200 count as 200."`
}

type ListOutput struct {
	Body Page
}

type Page struct {
	Items      []Event `json:"items"`
	NextBefore *int64  `json:"next_before" doc:"Pass it as before to read the next page; null on the last page."`
}

type Event struct {
	Seq         int64           `json:"seq" doc:"The hub's sequence number; newer events have higher numbers."`
	Plugin      string          `json:"plugin"`
	DisplayName string          `json:"display_name"`
	ID          string          `json:"id" doc:"The plugin's id for the event."`
	Kind        string          `json:"kind"`
	Severity    string          `json:"severity" enum:"info,urgent"`
	OccurredAt  time.Time       `json:"occurred_at"`
	DetectedAt  time.Time       `json:"detected_at" doc:"When the hub stored the event."`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	URL         string          `json:"url"`
	Payload     json.RawMessage `json:"payload,omitempty" doc:"The structured data the plugin attached, as it sent it."`
}

type Filter struct {
	Plugin string
	Kind   string
	Before int64
	Limit  int
}
