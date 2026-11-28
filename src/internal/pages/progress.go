package pages

import (
	"net/http"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

type progressView struct {
	Outcome outcome
	Ended   bool
	Buttons buttons
}

type pollFailure struct {
	Message string
	Buttons buttons
}

// followRestart answers the poll of a restart. htmx gets only the outcome, and nothing while a
// restart it has already shown goes on, so a screen reader does not hear it every second.
func (s *site) followRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.known(name) {
		s.fail(w, r, http.StatusNotFound, msgUnknown)
		return
	}
	if !isHTMX(r) {
		l, err := s.load(r.Context(), name, access.FromContext(r.Context()), true)
		if err != nil {
			s.failToLoad(w, r, name, err, msgDatabaseRead)
			return
		}
		e := buildEditor(name, l.view, l.progress, nil, nil)
		e.Outcome = resultOf(name, l.progress)
		s.wholePage(w, r, http.StatusOK, name, l, e)
		return
	}
	p, err := s.deps.Settings.Progress(r.Context(), name)
	if err != nil {
		s.failPoll(w, r, name, err)
		return
	}
	restarting := p.State == monitorconfig.ProgressRestarting
	if restarting && r.URL.Query().Get("shown") == "restarting" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.fragment(w, http.StatusOK, "progress", progressView{Outcome: resultOf(name, p), Ended: !restarting, Buttons: buttons{OutOfBand: true}})
}

// failPoll answers in place of the element that polls, so polling stops; the save buttons
// held while the restart went on come back with the message.
func (s *site) failPoll(w http.ResponseWriter, r *http.Request, name string, err error) {
	status, text, ok := s.loadFailure(r, name, err, msgDatabaseRead)
	if !ok {
		return
	}
	s.fragment(w, status, "poll-failure", pollFailure{Message: text, Buttons: buttons{OutOfBand: true}})
}

// resultOf is what the restart came to, as the supervisor saw it.
func resultOf(name string, p monitorconfig.Progress) outcome {
	o := outcome{
		Monitor:    name,
		Restarting: p.State == monitorconfig.ProgressRestarting,
		Settled:    p.State,
		Said:       p.Message,
		Reason:     p.Reason,
		Phase:      phaseText(p.Phase),
	}
	o.State, o.StateText = monitorState(monitors.StateName(p.MonitorState))
	return o
}
