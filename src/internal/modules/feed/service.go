package feed

import (
	"bytes"
	"cmp"
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"time"
)

const maxItems = 100

type Service struct {
	repo Repository
	opts Options
}

func NewService(repo Repository, opts Options) *Service {
	return &Service{repo: repo, opts: opts}
}

// Render lists only the events of the visible monitors.
func (s *Service) Render(ctx context.Context, monitor string, visible []string) ([]byte, error) {
	events, err := s.repo.RecentEvents(ctx, monitor, visible, maxItems)
	if err != nil {
		return nil, fmt.Errorf("feed: read the events: %w", err)
	}
	channel := Channel{Title: "monitor-hub", Link: s.opts.PublicURL, Description: "Events from every monitor-hub monitor", Items: make([]Item, 0, len(events))}
	if monitor != "" {
		channel.Title += ": " + monitor
		channel.Description = "Events from the " + monitor + " monitor"
	}
	for _, e := range events {
		channel.Items = append(channel.Items, item(e))
	}
	var body bytes.Buffer
	body.WriteString(xml.Header)
	if err := xml.NewEncoder(&body).Encode(RSS{Version: "2.0", Channel: channel}); err != nil {
		return nil, fmt.Errorf("feed: write the RSS document: %w", err)
	}
	return body.Bytes(), nil
}

func item(e Event) Item {
	return Item{
		Title:       cmp.Or(e.DisplayName, e.Monitor) + ": " + e.Title,
		Link:        webLink(e.URL),
		GUID:        GUID{IsPermaLink: "false", Value: e.Monitor + ":" + e.ID},
		PubDate:     e.OccurredAt.UTC().Format(time.RFC1123Z),
		Category:    e.Kind,
		Description: e.Body,
	}
}

func webLink(address string) string {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return address
}
