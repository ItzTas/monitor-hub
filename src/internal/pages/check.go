package pages

import (
	"errors"
	"net/http"

	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
)

type checkView struct {
	Text string
	Said string
}

type checkPage struct {
	Title  string
	Result checkView
}

// checkNow asks the monitor for a check. Without htmx, only a person who may configure the
// monitor gets its page back; anyone else gets the result alone.
func (s *site) checkNow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.known(name) {
		s.fail(w, r, http.StatusNotFound, msgUnknown)
		return
	}
	p := access.FromContext(r.Context())
	whole := !isHTMX(r) && p.Can(access.Configure, name)
	var (
		g errgroup.Group
		l loaded
	)
	if whole {
		g.Go(func() (err error) { l, err = s.load(r.Context(), name, p, true); return err })
	}
	result, err := s.deps.Monitors.Check(r.Context(), name)
	loadErr := g.Wait()
	view, status, ok := s.checkAnswer(w, r, name, result, err)
	if !ok {
		return
	}
	if isHTMX(r) {
		s.fragment(w, status, "check-result", view)
		return
	}
	if !whole {
		s.page(w, r, status, "check", name, checkPage{Title: name, Result: view})
		return
	}
	if loadErr != nil {
		s.failToLoad(w, r, name, loadErr, msgDatabaseRead)
		return
	}
	l.check = &view
	s.wholePage(w, r, status, name, l, buildEditor(name, l.view, l.progress, nil, nil))
}

// checkAnswer is false when it has answered already, or when the client left.
func (s *site) checkAnswer(w http.ResponseWriter, r *http.Request, name string, result monitors.CheckResult, err error) (checkView, int, bool) {
	if err == nil {
		text := map[bool]string{true: "The monitor started a check.", false: "The monitor did not start a check."}[result.Accepted]
		return checkView{Text: text, Said: result.Message}, http.StatusOK, true
	}
	if r.Context().Err() != nil {
		return checkView{}, 0, false
	}
	if errors.Is(err, monitors.ErrNotRunning) {
		return checkView{Text: "The monitor is not running, so it cannot check now."}, http.StatusOK, true
	}
	if errors.Is(err, monitors.ErrUnknown) {
		s.fail(w, r, http.StatusNotFound, msgUnknown)
		return checkView{}, 0, false
	}
	s.deps.Log.WarnContext(r.Context(), "the monitor did not answer the check", "monitor", name, "error", err)
	return checkView{Text: "The monitor did not answer. The hub's log has the details."}, http.StatusBadGateway, true
}
