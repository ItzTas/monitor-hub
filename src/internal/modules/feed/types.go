package feed

import (
	"encoding/xml"
	"time"
)

type Options struct {
	Token     string
	PublicURL string
}

type Event struct {
	Plugin      string
	DisplayName string
	ID          string
	Kind        string
	Title       string
	Body        string
	URL         string
	OccurredAt  time.Time
}

type RSS struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Channel Channel  `xml:"channel"`
}

type Channel struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	Items       []Item `xml:"item"`
}

type Item struct {
	Title       string `xml:"title"`
	Link        string `xml:"link,omitempty"`
	GUID        GUID   `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Category    string `xml:"category,omitempty"`
	Description string `xml:"description,omitempty"`
}

type GUID struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}
