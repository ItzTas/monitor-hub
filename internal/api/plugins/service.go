package plugins

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

const checkTimeout = 10 * time.Second

var (
	ErrUnknown    = errors.New("no plugin has this name")
	ErrNotRunning = errors.New("the plugin is not running")
)

type Supervisor interface {
	Names() []string
	Client(name string) (pluginpb.PluginClient, bool)
}

type Service struct {
	repo       Repository
	supervisor Supervisor
}

func NewService(repo Repository, supervisor Supervisor) *Service {
	return &Service{repo: repo, supervisor: supervisor}
}

func (s *Service) List(ctx context.Context) ([]Plugin, error) {
	rows, err := s.repo.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]Plugin{}
	for _, name := range s.supervisor.Names() {
		byName[name] = Plugin{Name: name, State: stateName(0)}
	}
	for _, row := range rows {
		plugin := row.Plugin
		plugin.State = stateName(row.State)
		byName[row.Name] = plugin
	}
	plugins := make([]Plugin, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		plugin := byName[name]
		_, plugin.Running = s.supervisor.Client(name)
		plugins = append(plugins, plugin)
	}
	return plugins, nil
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
	answer, err := client.Check(ctx, &pluginpb.CheckRequest{})
	if err != nil {
		return CheckResult{}, fmt.Errorf("check %s: %w", name, err)
	}
	return CheckResult{Accepted: answer.GetAccepted(), Message: answer.GetMessage()}, nil
}

func stateName(state int16) string {
	name, known := pluginpb.State_name[int32(state)]
	if !known || state == int16(pluginpb.State_STATE_UNSPECIFIED) {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "STATE_"))
}
