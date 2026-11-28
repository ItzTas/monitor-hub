package pages

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

type monitorPage struct {
	Name   string
	Title  string
	Status statusView
	Editor editor
}

// statusView shows what List says only for a monitor in it, which means one the person may view.
type statusView struct {
	Monitor     string
	Listed      bool
	Running     bool
	State       string
	StateText   string
	Message     string
	LastSuccess string
	NextCheck   string
	ShowStart   bool
	Phase       string
	Reason      string
	CanCheck    bool
}

// may refuses before anything is looked up, so the refusal does not tell whether the monitor exists.
func (s *site) may(action access.Action, next http.HandlerFunc) http.HandlerFunc {
	refusals := map[access.Action]string{access.Configure: msgNoConfigure, access.Check: msgNoCheck}
	return func(w http.ResponseWriter, r *http.Request) {
		if !access.FromContext(r.Context()).Can(action, r.PathValue("name")) {
			s.fail(w, r, http.StatusForbidden, refusals[action])
			return
		}
		next(w, r)
	}
}

func (s *site) known(name string) bool {
	return slices.ContainsFunc(s.deps.Entries, func(e catalog.Entry) bool { return e.Manifest.Name == name })
}

func (s *site) monitor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.known(name) {
		s.fail(w, r, http.StatusNotFound, msgUnknown)
		return
	}
	p := access.FromContext(r.Context())
	var (
		view     monitorconfig.View
		progress monitorconfig.Progress
		start    monitorconfig.StartReport
		listed   []monitors.Monitor
	)
	g, ctx := errgroup.WithContext(r.Context())
	g.Go(func() (err error) { view, err = s.deps.Settings.Read(ctx, name); return err })
	g.Go(func() (err error) { progress, err = s.deps.Settings.Progress(ctx, name); return err })
	g.Go(func() (err error) { start, err = s.deps.Settings.LastStart(ctx, name); return err })
	g.Go(func() (err error) { listed, err = s.deps.Monitors.List(ctx, p); return err })
	if err := g.Wait(); err != nil {
		s.failToLoad(w, r, name, err)
		return
	}
	var shown *monitors.Monitor
	if i := slices.IndexFunc(listed, func(m monitors.Monitor) bool { return m.Name == name }); i >= 0 {
		shown = &listed[i]
	}
	page := monitorPage{Name: name, Title: name, Status: newStatus(name, p, start, shown), Editor: buildEditor(name, view, progress, nil, nil)}
	if shown != nil && shown.DisplayName != "" {
		page.Title = shown.DisplayName
	}
	s.page(w, r, http.StatusOK, "monitor", page.Title, page)
}

func (s *site) failToLoad(w http.ResponseWriter, r *http.Request, name string, err error) {
	if r.Context().Err() != nil {
		return
	}
	if errors.Is(err, monitorconfig.ErrUnknown) {
		s.fail(w, r, http.StatusNotFound, msgUnknown)
		return
	}
	s.deps.Log.ErrorContext(r.Context(), "could not read a monitor for the settings page", "monitor", name, "error", err)
	s.fail(w, r, http.StatusServiceUnavailable, msgDatabaseRead)
}

// newVariable is one more blank row for "Add another variable", which only htmx asks for.
func (s *site) newVariable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.known(name) {
		s.fail(w, r, http.StatusNotFound, msgUnknown)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/settings/"+name, http.StatusSeeOther)
		return
	}
	s.fragment(w, http.StatusOK, "new-variable-row", nil)
}

// newStatus takes the monitor as List gave it, or nil when List left it out.
func newStatus(name string, p access.Principal, start monitorconfig.StartReport, m *monitors.Monitor) statusView {
	phases := map[supervisor.Phase]string{
		supervisor.PhaseNotStarted: "Not started yet",
		supervisor.PhaseStarting:   "Starting",
		supervisor.PhaseRunning:    "Running",
		supervisor.PhaseFailed:     "Failed to start",
		supervisor.PhaseExited:     "Stopped after starting",
	}
	v := statusView{Monitor: name, CanCheck: p.Can(access.Check, name), Reason: start.Reason}
	v.Phase = phases[start.Phase]
	if v.Phase == "" {
		v.Phase = phases[supervisor.PhaseNotStarted]
	}
	if m != nil {
		v.Listed, v.Running, v.Message = true, m.Running, m.Message
		v.State, v.StateText = monitorState(m.State)
		v.LastSuccess, v.NextCheck = localTime(m.LastSuccessAt), localTime(m.NextCheckAt)
	}
	v.ShowStart = !v.Listed || !v.Running
	return v
}

func localTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}
