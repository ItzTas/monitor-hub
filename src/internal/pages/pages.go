// Package pages serves the hub's settings pages: signing in and out, the monitors a person
// may configure, and the configuration form of each one.
package pages

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
	"github.com/ItzTas/monitor-hub/src/internal/api/session"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

type Settings interface {
	Read(ctx context.Context, name string) (monitorconfig.View, error)
	Save(ctx context.Context, name string, change monitorconfig.Change) (monitorconfig.SaveResult, error)
	Progress(ctx context.Context, name string) (monitorconfig.Progress, error)
	LastStart(ctx context.Context, name string) (monitorconfig.StartReport, error)
}

type Monitors interface {
	List(ctx context.Context, p access.Principal) ([]monitors.Monitor, error)
	Check(ctx context.Context, name string) (monitors.CheckResult, error)
}

type Deps struct {
	Entries   []catalog.Entry
	Settings  Settings
	Monitors  Monitors
	Accounts  Accounts
	Logins    *session.Logins
	PublicURL string
	Log       *slog.Logger
}

type site struct {
	deps      Deps
	assets    assets
	pages     map[string]*template.Template
	fragments *template.Template
}

func Register(mux *http.ServeMux, deps Deps) {
	s := newSite(deps)
	routes := map[string]http.Handler{
		"GET /static/{file}":                 s.assets,
		"GET /settings/login":                http.HandlerFunc(s.signInPage),
		"POST /settings/login":               s.fromHub(s.signIn),
		"POST /settings/logout":              s.fromHub(s.signOut),
		"GET /settings":                      s.authed(s.list),
		"GET /settings/{name}":               s.authed(s.may(access.Configure, s.monitor)),
		"GET /settings/{name}/variables/new": s.authed(s.may(access.Configure, s.newVariable)),
		"POST /settings/{name}/form":         s.changes(s.may(access.Configure, s.edit)),
	}
	for pattern, handler := range routes {
		mux.Handle(pattern, secureHeaders(handler))
	}
}

// newSite panics on a broken embedded file: that is a bug of the build, not of the request.
func newSite(deps Deps) *site {
	files, err := loadAssets()
	if err != nil {
		panic(err)
	}
	s := &site{deps: deps, assets: files}
	s.fragments, s.pages = parseTemplates(template.FuncMap{"asset": files.url})
	return s
}
