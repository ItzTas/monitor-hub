package pages

import (
	"net/http"

	"github.com/ItzTas/monitor-hub/src/internal/access"
)

// edit applies one list operation to the sent form and draws the form again. It never saves.
func (s *site) edit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.known(name) {
		s.fail(w, r, http.StatusNotFound, msgUnknown)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.failToRead(w, r, err)
		return
	}
	sent := r.PostForm.Get("declaration")
	if sent == "" {
		s.fail(w, r, http.StatusBadRequest, msgIncomplete)
		return
	}
	l, err := s.load(r.Context(), name, access.FromContext(r.Context()), !isHTMX(r))
	if err != nil {
		s.failToLoad(w, r, name, err)
		return
	}
	if sent != declarationKey(l.view) {
		e := buildEditor(name, l.view, l.progress, nil, nil)
		e.Outcome.Message = msgDeclaration
		s.drawEditor(w, r, http.StatusConflict, name, l, e)
		return
	}
	sub := applyOp(l.view.Declaration, parseSubmission(l.view, r.PostForm), r.PostForm.Get("op"))
	e := buildEditor(name, l.view, l.progress, &sub, nil)
	e.Outcome.Message = msgNotSaved
	s.drawEditor(w, r, http.StatusOK, name, l, e)
}

// drawEditor answers htmx with the editor alone, and a page load with the whole monitor page.
func (s *site) drawEditor(w http.ResponseWriter, r *http.Request, status int, name string, l loaded, e editor) {
	if isHTMX(r) {
		s.fragment(w, status, "editor", e)
		return
	}
	s.wholePage(w, r, status, name, l, e)
}
