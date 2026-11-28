package monitorconfig

import (
	"context"
	"errors"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

const (
	settleLimit   = 75 * time.Second
	statusTimeout = 5 * time.Second
	reasonLimit   = 500
)

type ProgressState string

const (
	ProgressNone       ProgressState = "none"
	ProgressRestarting ProgressState = "restarting"
	ProgressUp         ProgressState = "up"
	ProgressFailed     ProgressState = "failed"
	ProgressNoAnswer   ProgressState = "no_answer"
)

// Progress is how the restart the last save asked for is going.
type Progress struct {
	State        ProgressState
	Request      uint64
	RequestedAt  *time.Time
	Phase        supervisor.Phase
	MonitorState monitorpb.State
	Message      string
	Reason       string
}

// Progress follows the last restart a save asked for since the hub started. The reason of a
// failed start leaves out the secrets of the saved configuration.
func (s *Service) Progress(ctx context.Context, name string) (Progress, error) {
	entry, ok := s.entries[name]
	if !ok {
		return Progress{}, ErrUnknown
	}
	_, a, err := s.load(ctx, entry)
	if err != nil {
		return Progress{}, errors.Join(ErrDatabase, err)
	}
	p := s.progress(ctx, name)
	p.Reason = cut(redact(p.Reason, a.Secrets), reasonLimit)
	return p, nil
}

// StartReport is how the monitor's last start went.
type StartReport struct {
	Phase  supervisor.Phase
	Reason string
}

// LastStart reports the monitor's last start, with the secrets of the saved configuration
// taken out of the reason.
func (s *Service) LastStart(ctx context.Context, name string) (StartReport, error) {
	entry, ok := s.entries[name]
	if !ok {
		return StartReport{}, ErrUnknown
	}
	a := s.deps.Supervisor.LastAttempt(name)
	if a.Reason == "" {
		return StartReport{Phase: a.Phase}, nil
	}
	// Without a declaration nothing is secret.
	if entry.Settings == nil {
		return StartReport{Phase: a.Phase, Reason: cut(a.Reason, reasonLimit)}, nil
	}
	_, assembly, err := s.load(ctx, entry)
	if err != nil {
		return StartReport{}, errors.Join(ErrDatabase, err)
	}
	return StartReport{Phase: a.Phase, Reason: cut(redact(a.Reason, assembly.Secrets), reasonLimit)}, nil
}

// progress is Progress with the reason as the supervisor gave it.
func (s *Service) progress(ctx context.Context, name string) Progress {
	s.mu.Lock()
	r, ok := s.requests[name]
	s.mu.Unlock()
	a := s.deps.Supervisor.LastAttempt(name)
	if !ok {
		return Progress{State: ProgressNone, Phase: a.Phase}
	}
	p := Progress{Request: r.number, RequestedAt: &r.at, Phase: a.Phase}
	settled := a.Settled >= r.number
	if settled && a.Phase != supervisor.PhaseRunning {
		p.State = ProgressFailed
		p.Reason = a.Reason
		return p
	}
	if settled {
		if status, ok := s.status(ctx, name); ok {
			p.State = ProgressUp
			p.MonitorState = status.GetState()
			p.Message = status.GetMessage()
			return p
		}
	}
	p.State = ProgressRestarting
	if s.deps.Now().Sub(r.at) >= settleLimit {
		p.State = ProgressNoAnswer
	}
	return p
}

func (s *Service) status(ctx context.Context, name string) (*monitorpb.StatusResponse, bool) {
	client, ok := s.deps.Supervisor.Client(name)
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	status, err := client.Status(ctx, &monitorpb.StatusRequest{})
	return status, err == nil
}

func cut(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
