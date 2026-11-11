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

func (s *Service) Render(ctx context.Context, plugin string) ([]byte, error) {
	events, err := s.repo.RecentEvents(ctx, plugin, maxItems)
	if err != nil {
		return nil, fmt.Errorf("feed: read the events: %w", err)
	}
	channel := Channel{Title: "monitor-hub", Link: s.opts.PublicURL, Description: "Events from every monitor-hub plugin", Items: make([]Item, 0, len(events))}
	if plugin != "" {
		channel.Title += ": " + plugin
		channel.Description = "Events from the " + plugin + " plugin"
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
		Title:       cmp.Or(e.DisplayName, e.Plugin) + ": " + e.Title,
		Link:        webLink(e.URL),
		GUID:        GUID{IsPermaLink: "false", Value: e.Plugin + ":" + e.ID},
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
