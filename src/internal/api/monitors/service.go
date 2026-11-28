package monitors

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/access"
)

const checkTimeout = 10 * time.Second

var (
	ErrUnknown    = errors.New("no monitor has this name")
	ErrNotRunning = errors.New("the monitor is not running")
)

type Supervisor interface {
	Names() []string
	Client(name string) (monitorpb.MonitorClient, bool)
}

type Service struct {
	repo       Repository
	supervisor Supervisor
}

func NewService(repo Repository, supervisor Supervisor) *Service {
	return &Service{repo: repo, supervisor: supervisor}
}

// List gives only the monitors p can view.
func (s *Service) List(ctx context.Context, p access.Principal) ([]Monitor, error) {
	rows, err := s.repo.ListMonitors(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]Monitor{}
	for _, name := range s.supervisor.Names() {
		byName[name] = Monitor{Name: name, State: StateName(monitorpb.State_STATE_UNSPECIFIED)}
	}
	for _, row := range rows {
		monitor := row.Monitor
		monitor.State = StateName(monitorpb.State(row.State))
		byName[row.Name] = monitor
	}
	visible := access.Visible(p, slices.Sorted(maps.Keys(byName)))
	monitors := make([]Monitor, 0, len(visible))
	for _, name := range visible {
		monitor := byName[name]
		_, monitor.Running = s.supervisor.Client(name)
		monitors = append(monitors, monitor)
	}
	return monitors, nil
}

func (s *Service) Check(ctx context.Context, name string) (CheckResult, error) {
	if !slices.Contains(s.supervisor.Names(), name) {
		return CheckResult{}, ErrUnknown
	}
	client, ok := s.supervisor.Client(name)
	if !ok {
		return CheckResult{}, ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	answer, err := client.Check(ctx, &monitorpb.CheckRequest{})
	if err != nil {
		return CheckResult{}, fmt.Errorf("check %s: %w", name, err)
	}
	return CheckResult{Accepted: answer.GetAccepted(), Message: answer.GetMessage()}, nil
}

func StateName(state monitorpb.State) string {
	name, known := monitorpb.State_name[int32(state)]
	if !known || state == monitorpb.State_STATE_UNSPECIFIED {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "STATE_"))
}
