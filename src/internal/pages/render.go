package pages

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

//go:embed templates
var templateFiles embed.FS

const (
	msgOrigin       = "The hub refused this request because it came from another site."
	msgDatabaseRead = "The hub's database cannot be reached, so the settings cannot be shown."
	msgTooLarge     = "The form is too large. The hub saved nothing."
	msgIncomplete   = "The form is incomplete. Reload the page and try again."
	msgInternal     = "Something went wrong. The hub's log has the details."
	msgUnknown      = "No monitor has this name."
	msgNoConfigure  = "You do not have permission to change this monitor's configuration. An admin can give you the configure permission."
	msgNoCheck      = "You do not have permission to ask this monitor for a check."
	msgDeclaration  = "This monitor's settings changed since the page was loaded. The form now shows the current ones. The hub saved nothing."
	msgNotSaved     = "Nothing is saved yet. Choose Save and restart to keep these changes."
	msgRevision     = "The saved values changed in another tab or window. The form now shows them. The hub saved nothing, so make your changes again."
	msgRestarting   = "The monitor is still restarting after the last save. Wait until it settles, then save again. The hub saved nothing."
	msgBusy         = "Another save of this monitor is in progress. The hub saved nothing."
	msgDatabaseSave = "The hub's database cannot be reached. The hub saved nothing."
	msgValidator    = "The monitor could not check the values, so the hub saved nothing. Reason: "
	msgChanged      = "Saved. The monitor's environment did not change, so the hub did not restart it."
	msgNothing      = "Nothing to save: no value changed."
	msgReread       = "The hub saved the values but could not read them back. Reload the page."
	msgTooManyItems = "The lists in this configuration hold more than 1000 items in all, which is too many for this page. Change this field through the hub's API."
)

type layout struct {
	Title   string
	Login   string // empty without a session
	Content any
}

type errorPage struct {
	Title   string
	Message string
}

// parseTemplates gives the partials, which fragments run from, and one set per page:
// the partials and the page's own file, which defines "content".
func parseTemplates(funcs template.FuncMap) (*template.Template, map[string]*template.Template) {
	partials := template.Must(template.New("partials").Funcs(funcs).ParseFS(templateFiles,
		"templates/layout.html", "templates/message.html", "templates/status.html", "templates/editor.html",
		"templates/field.html", "templates/variables.html", "templates/ignored.html", "templates/outcome.html"))
	pages := map[string]*template.Template{}
	for _, name := range []string{"login", "monitors", "monitor", "check", "error"} {
		pages[name] = template.Must(template.Must(partials.Clone()).ParseFS(templateFiles, "templates/"+name+".html"))
	}
	return partials, pages
}

func (s *site) page(w http.ResponseWriter, r *http.Request, status int, name, title string, content any) {
	data := layout{Title: title, Login: access.FromContext(r.Context()).Login(), Content: content}
	s.write(w, status, s.pages[name], "layout", data)
}

func (s *site) fragment(w http.ResponseWriter, status int, name string, data any) {
	s.write(w, status, s.fragments, name, data)
}

// write renders into a buffer first, so a template error never leaves half a page.
func (s *site) write(w http.ResponseWriter, status int, set *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, name, data); err != nil {
		s.deps.Log.Error("could not render a settings page", "template", name, "error", err)
		http.Error(w, msgInternal, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// fail answers htmx with only the message, in place of the outcome, and a page
// load with the error page. A refused poll also gives back the save buttons the
// restart held.
func (s *site) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	if isHTMX(r) {
		w.Header().Set("HX-Retarget", "#outcome")
		w.Header().Set("HX-Reswap", "innerHTML")
		if r.Pattern == progressRoute {
			s.fragment(w, status, "poll-failure", pollFailure{Message: message, Buttons: buttons{OutOfBand: true}})
			return
		}
		s.fragment(w, status, "message", message)
		return
	}
	titles := map[int]string{
		http.StatusBadRequest:            "Incomplete form",
		http.StatusForbidden:             "Request refused",
		http.StatusNotFound:              "Not found",
		http.StatusConflict:              "Not saved",
		http.StatusRequestEntityTooLarge: "Form too large",
		http.StatusInternalServerError:   "Something went wrong",
		http.StatusBadGateway:            "The monitor could not check the values",
		http.StatusServiceUnavailable:    "Database unavailable",
	}
	title := titles[status]
	s.page(w, r, status, "error", title, errorPage{Title: title, Message: message})
}

// failWith is fail for a save refused while the last one restarts: htmx also gets the element
// that polls the restart.
func (s *site) failWith(w http.ResponseWriter, r *http.Request, status int, message string, progress monitorconfig.Progress) {
	if !isHTMX(r) || progress.State != monitorconfig.ProgressRestarting {
		s.fail(w, r, status, message)
		return
	}
	w.Header().Set("HX-Retarget", "#outcome")
	w.Header().Set("HX-Reswap", "innerHTML")
	s.fragment(w, status, "outcome", outcome{Monitor: r.PathValue("name"), Waiting: true, Message: message})
}

func (s *site) fromHub(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r, s.deps.PublicURL) {
			s.fail(w, r, http.StatusForbidden, msgOrigin)
			return
		}
		next(w, r)
	}
}

// changes guards a route that changes state: the origin, then the session; the route's own
// permission check comes after.
func (s *site) changes(next http.HandlerFunc) http.HandlerFunc {
	return s.fromHub(s.authed(next))
}

// authed lets in only a session, and hands next the principal in the context.
func (s *site) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := principal(r, s.deps.Accounts)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			s.deps.Log.ErrorContext(r.Context(), "could not read the session of a settings page", "error", err)
			s.fail(w, r, http.StatusServiceUnavailable, msgDatabaseRead)
			return
		}
		if p.Kind() == access.Nobody {
			signInFirst(w, r)
			return
		}
		next(w, r.WithContext(access.WithPrincipal(r.Context(), p)))
	}
}

// signInFirst sends htmx to the sign-in page by header: it would swap a redirect's body in place.
func signInFirst(w http.ResponseWriter, r *http.Request) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/settings/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/settings/login", http.StatusSeeOther)
}
