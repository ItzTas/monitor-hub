package monitors

import "time"

type Monitor struct {
	Name               string     `json:"name"`
	DisplayName        string     `json:"display_name"`
	Version            string     `json:"version"`
	Running            bool       `json:"running" doc:"Whether the hub is connected to the monitor's process."`
	State              string     `json:"state" enum:"idle,checking,paused,failing,unknown" doc:"What the monitor last said it was doing."`
	Message            string     `json:"message"`
	LastCheckStartedAt *time.Time `json:"last_check_started_at"`
	LastSuccessAt      *time.Time `json:"last_success_at"`
	NextCheckAt        *time.Time `json:"next_check_at"`
	StatusSeenAt       *time.Time `json:"status_seen_at" doc:"When the hub last read the monitor's status."`
}

type stored struct {
	Monitor
	State int16
}

type ListOutput struct {
	Body struct {
		Items []Monitor `json:"items"`
	}
}

type CheckInput struct {
	Name string `path:"name" doc:"The monitor's name."`
}

type CheckOutput struct {
	Body CheckResult
}

type CheckResult struct {
	Accepted bool   `json:"accepted" doc:"Whether the monitor started a check."`
	Message  string `json:"message" doc:"What the monitor said, for example why it refused."`
}
