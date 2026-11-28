package pages

import (
	"net/http"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

// monitorCard shows the state only of a monitor the person may view: List leaves out the rest.
type monitorCard struct {
	Name      string
	Title     string
	Declared  bool
	Shown     bool
	Running   bool
	State     string
	StateText string
	Message   string
}

func (s *site) list(w http.ResponseWriter, r *http.Request) {
	p := access.FromContext(r.Context())
	listed, err := s.deps.Monitors.List(r.Context(), p)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.deps.Log.ErrorContext(r.Context(), "could not list the monitors for the settings page", "error", err)
		s.fail(w, r, http.StatusServiceUnavailable, msgDatabaseRead)
		return
	}
	byName := make(map[string]monitors.Monitor, len(listed))
	for _, m := range listed {
		byName[m.Name] = m
	}
	cards := []monitorCard{}
	for _, entry := range s.deps.Entries {
		if p.Can(access.Configure, entry.Manifest.Name) {
			cards = append(cards, newMonitorCard(entry, byName))
		}
	}
	s.page(w, r, http.StatusOK, "monitors", "Monitors", cards)
}

func newMonitorCard(entry catalog.Entry, listed map[string]monitors.Monitor) monitorCard {
	name := entry.Manifest.Name
	c := monitorCard{Name: name, Title: name, Declared: entry.Settings != nil}
	m, ok := listed[name]
	if !ok {
		return c
	}
	states := map[string]string{"idle": "Idle", "checking": "Checking", "paused": "Paused", "failing": "Failing", "unknown": "Unknown"}
	c.Shown, c.Running, c.Message, c.State = true, m.Running, m.Message, m.State
	if _, known := states[m.State]; !known {
		c.State = "unknown"
	}
	c.StateText = states[c.State]
	if m.DisplayName != "" {
		c.Title = m.DisplayName
	}
	return c
}
