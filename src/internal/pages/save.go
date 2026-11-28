package pages

import (
	"errors"
	"net/http"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

// saveForm checks the sent form against a fresh Read, then saves what it changes.
func (s *site) saveForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.known(name) {
		s.fail(w, r, http.StatusNotFound, msgUnknown)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.failToRead(w, r, err)
		return
	}
	if r.PostForm.Get("revision") == "" || r.PostForm.Get("declaration") == "" {
		s.fail(w, r, http.StatusBadRequest, msgIncomplete)
		return
	}
	l, err := s.load(r.Context(), name, access.FromContext(r.Context()), !isHTMX(r))
	if err != nil {
		s.failToLoad(w, r, name, err, msgDatabaseSave)
		return
	}
	sub := parseSubmission(l.view, r.PostForm)
	change, problems, err := changeFrom(l.view, sub)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, msgIncomplete)
		return
	}
	if sub.Declaration != declarationKey(l.view) {
		s.refresh(w, r, name, l, msgDeclaration)
		return
	}
	// Before any value is checked, so a 422 never sends old values back with a new revision.
	if *change.Revision != l.view.Revision {
		s.refresh(w, r, name, l, msgRevision)
		return
	}
	if len(problems) > 0 {
		s.drawEditor(w, r, http.StatusUnprocessableEntity, name, l, buildEditor(name, l.view, l.progress, &sub, problems))
		return
	}
	result, err := s.deps.Settings.Save(r.Context(), name, change)
	if err != nil {
		s.failToSave(w, r, name, l, sub, err)
		return
	}
	s.saved(w, r, name, result)
}

// refresh answers a form drawn from an older View with the current one: the hub saved nothing.
func (s *site) refresh(w http.ResponseWriter, r *http.Request, name string, l loaded, message string) {
	e := buildEditor(name, l.view, l.progress, nil, nil)
	e.Outcome.Message = message
	s.drawEditor(w, r, http.StatusConflict, name, l, e)
}

func (s *site) failToSave(w http.ResponseWriter, r *http.Request, name string, l loaded, sub submission, err error) {
	ctx := r.Context()
	if ctx.Err() != nil {
		return
	}
	if problems, ok := errors.AsType[*monitorconfig.ProblemsError](err); ok {
		e := buildEditor(name, l.view, l.progress, &sub, formPaths(problems.Problems, sub.Indexes))
		s.drawEditor(w, r, http.StatusUnprocessableEntity, name, l, e)
		return
	}
	if validator, ok := errors.AsType[*monitorconfig.ValidatorError](err); ok {
		s.fail(w, r, http.StatusBadGateway, msgValidator+validator.Reason)
		return
	}
	if errors.Is(err, monitorconfig.ErrStale) {
		s.refreshAfterStale(w, r, name)
		return
	}
	if errors.Is(err, monitorconfig.ErrRestarting) {
		// Without the progress, the refusal goes without the element that polls it.
		progress, _ := s.deps.Settings.Progress(ctx, name)
		s.failWith(w, r, http.StatusConflict, msgRestarting, progress)
		return
	}
	known := []struct {
		err     error
		status  int
		message string
	}{
		{monitorconfig.ErrBusy, http.StatusConflict, msgBusy},
		{monitorconfig.ErrUnknown, http.StatusNotFound, msgUnknown},
		{monitorconfig.ErrDatabase, http.StatusServiceUnavailable, msgDatabaseSave},
	}
	status, message := http.StatusInternalServerError, msgInternal
	for _, k := range known {
		if errors.Is(err, k.err) {
			status, message = k.status, k.message
			break
		}
	}
	if status >= http.StatusInternalServerError {
		s.deps.Log.ErrorContext(ctx, "could not save monitor configuration", "monitor", name, "error", err)
	}
	s.fail(w, r, status, message)
}

func (s *site) refreshAfterStale(w http.ResponseWriter, r *http.Request, name string) {
	l, err := s.load(r.Context(), name, access.FromContext(r.Context()), !isHTMX(r))
	if err != nil {
		s.failToLoad(w, r, name, err, msgDatabaseSave)
		return
	}
	s.refresh(w, r, name, l, msgRevision)
}

// saved draws the form again from what is saved now, with what the save came to.
func (s *site) saved(w http.ResponseWriter, r *http.Request, name string, result monitorconfig.SaveResult) {
	l, err := s.load(r.Context(), name, access.FromContext(r.Context()), !isHTMX(r))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.deps.Log.ErrorContext(r.Context(), "could not read a monitor back after saving it", "monitor", name, "error", err)
		s.fail(w, r, http.StatusServiceUnavailable, msgReread)
		return
	}
	e := buildEditor(name, l.view, l.progress, nil, nil)
	e.Outcome.Focus = true
	switch {
	case result.Restarted:
		e.Outcome.Restarting, e.Buttons.Restarting = true, true
	case len(result.Changed) > 0:
		e.Outcome.Message = msgChanged
	default:
		e.Outcome.Message = msgNothing
	}
	s.drawEditor(w, r, http.StatusOK, name, l, e)
}
