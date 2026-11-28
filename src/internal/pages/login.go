package pages

import (
	"errors"
	"net/http"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

// signInView never carries the typed password back: only the login.
type signInView struct {
	Encrypted bool
	Login     string
	Wrong     string
}

func (s *site) signInPage(w http.ResponseWriter, r *http.Request) {
	if p, err := principal(r, s.deps.Accounts); err == nil && p.Kind() != access.Nobody {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	s.page(w, r, http.StatusOK, "login", "Sign in", signInView{Encrypted: isHTTPS(r)})
}

// signIn waits in the same queue as POST /api/session, so the page is no faster way to guess.
func (s *site) signIn(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.failToRead(w, r, err)
		return
	}
	login := r.PostForm.Get("login")
	var opened accounts.Session
	err := s.deps.Logins.Run(r.Context(), func() (err error) {
		opened, _, err = s.deps.Accounts.Login(r.Context(), login, r.PostForm.Get("password"))
		return err
	})
	if err == nil {
		c := gate.SessionCookie(opened.ID, opened.ExpiresAt, isHTTPS(r))
		http.SetCookie(w, &c)
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if errors.Is(err, accounts.ErrWrongPassword) {
		s.page(w, r, http.StatusUnauthorized, "login", "Sign in", signInView{Encrypted: isHTTPS(r), Login: login, Wrong: err.Error()})
		return
	}
	s.deps.Log.ErrorContext(r.Context(), "could not sign in from the settings page", "error", err)
	s.fail(w, r, http.StatusInternalServerError, msgInternal)
}

func (s *site) failToRead(w http.ResponseWriter, r *http.Request, err error) {
	if _, large := errors.AsType[*http.MaxBytesError](err); large {
		s.fail(w, r, http.StatusRequestEntityTooLarge, msgTooLarge)
		return
	}
	s.fail(w, r, http.StatusBadRequest, msgIncomplete)
}

func (s *site) signOut(w http.ResponseWriter, r *http.Request) {
	if err := s.endSession(r); err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.deps.Log.ErrorContext(r.Context(), "could not sign out from the settings page", "error", err)
		s.fail(w, r, http.StatusInternalServerError, msgInternal)
		return
	}
	c := gate.ClearedSessionCookie(isHTTPS(r))
	http.SetCookie(w, &c)
	http.Redirect(w, r, "/settings/login", http.StatusSeeOther)
}

func (s *site) endSession(r *http.Request) error {
	c, err := r.Cookie(gate.CookieName)
	if err != nil {
		return nil
	}
	return s.deps.Accounts.Logout(r.Context(), c.Value)
}
