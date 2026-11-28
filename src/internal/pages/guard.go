package pages

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

type Accounts interface {
	BySession(ctx context.Context, id string) (access.Principal, error)
	Login(ctx context.Context, login, password string) (accounts.Session, access.Principal, error)
	Logout(ctx context.Context, sessionID string) error
}

var _ Accounts = (*accounts.Service)(nil)

const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// principal reads only the session cookie: a token never opens a page. A
// cookie the hub no longer knows is nobody, not an error.
func principal(r *http.Request, people Accounts) (access.Principal, error) {
	c, err := r.Cookie(gate.CookieName)
	if err != nil {
		return access.Principal{}, nil
	}
	p, err := people.BySession(r.Context(), c.Value)
	if errors.Is(err, accounts.ErrUnauthenticated) {
		return access.Principal{}, nil
	}
	return p, err
}

func sameOrigin(r *http.Request, publicURL string) bool {
	return gate.AllowedOrigin(r.Header.Get("Origin"), r.Host, publicURL)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// isHTTPS follows the gate's rule for the Secure cookie, trusting the proxy's X-Forwarded-Proto.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
