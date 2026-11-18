package stream

import (
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/icons"
)

type Notification struct {
	Seq         int64      `json:"seq"`
	Monitor     string     `json:"monitor"`
	DisplayName string     `json:"display_name"`
	Kind        string     `json:"kind"`
	Urgent      bool       `json:"urgent"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	URL         string     `json:"url"`
	OccurredAt  time.Time  `json:"occurred_at"`
	DetectedAt  time.Time  `json:"detected_at"`
	Icon        icons.Icon `json:"icon"`
	MonitorIcon icons.Icon `json:"monitor_icon"`
}

type CaughtUp struct {
	Seq     int64 `json:"seq"`
	Backlog int   `json:"backlog"`
}

type Replaced struct{}
