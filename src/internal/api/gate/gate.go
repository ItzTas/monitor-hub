// Package gate tells who makes an API request and lets in only who each route allows.
package gate

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
)

type Level string

const (
	Anyone     Level = "anyone"
	Identified Level = "identified"
	Session    Level = "session"
)

const CookieName = "monitor_hub_session"

const (
	msgNobody      = "sign in or send a personal token"
	msgNeedSession = "this route needs a signed-in session"
	msgForbidden   = "you do not have permission for this"
	msgOrigin      = "the request comes from another site"
)

// Meta goes in huma.Operation.Metadata. The gate treats a route without it as Session.
func Meta(l Level) map[string]any {
	return map[string]any{"gate": l}
}

type Resolver interface {
	BySession(ctx context.Context, id string) (access.Principal, error)
	ByToken(ctx context.Context, secret string) (access.Principal, error)
}

type refusal struct {
	status int
	msg    string
}

func Middleware(api huma.API, resolver Resolver, publicURL string) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		level := routeLevel(ctx.Operation())
		p, refused, err := identify(ctx, resolver, level)
		if err != nil {
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "internal error", err)
			return
		}
		if refused == nil {
			refused = check(ctx, p, level, publicURL)
		}
		if refused == nil {
			next(huma.WithContext(ctx, access.WithPrincipal(ctx.Context(), p)))
			return
		}
		if refused.status == http.StatusUnauthorized {
			for name, values := range Challenge() {
				ctx.SetHeader(name, strings.Join(values, ", "))
			}
		}
		_ = huma.WriteErr(api, ctx, refused.status, refused.msg)
	}
}

// routeLevel takes Session for a route without a level or with one it does not know.
func routeLevel(op *huma.Operation) Level {
	level, _ := op.Metadata["gate"].(Level)
	if level == Anyone || level == Identified {
		return level
	}
	return Session
}

// identify reads the Authorization header, or else the session cookie. A
// credential that does not hold is refused and never taken for nobody, except
// a stale cookie on an Anyone route, so that it cannot block signing in.
func identify(ctx huma.Context, resolver Resolver, level Level) (access.Principal, *refusal, error) {
	invalid := &refusal{http.StatusUnauthorized, accounts.ErrUnauthenticated.Error()}
	if auth := ctx.Header("Authorization"); auth != "" {
		scheme, secret, _ := strings.Cut(auth, " ")
		if !strings.EqualFold(scheme, "Bearer") {
			return access.Principal{}, &refusal{http.StatusUnauthorized, msgNobody}, nil
		}
		p, err := resolver.ByToken(ctx.Context(), secret)
		if errors.Is(err, accounts.ErrUnauthenticated) {
			return access.Principal{}, invalid, nil
		}
		return p, nil, err
	}
	c, err := huma.ReadCookie(ctx, CookieName)
	if err != nil {
		return access.Principal{}, nil, nil
	}
	p, err := resolver.BySession(ctx.Context(), c.Value)
	if !errors.Is(err, accounts.ErrUnauthenticated) {
		return p, nil, err
	}
	cleared := ClearCookie(ctx)
	ctx.AppendHeader("Set-Cookie", cleared.String())
	if level == Anyone {
		return access.Principal{}, nil, nil
	}
	return access.Principal{}, invalid, nil
}

func check(ctx huma.Context, p access.Principal, level Level, publicURL string) *refusal {
	if p.Kind() == access.Nobody && level != Anyone {
		return &refusal{http.StatusUnauthorized, msgNobody}
	}
	if p.Kind() != access.Session && level == Session {
		return &refusal{http.StatusForbidden, msgNeedSession}
	}
	if !fromThisSite(ctx, p, publicURL) {
		return &refusal{http.StatusForbidden, msgOrigin}
	}
	return nil
}

// fromThisSite asks a change for an Origin of the hub itself, and a session for an
// Origin at all: browsers send one, so a session change without it is suspect.
func fromThisSite(ctx huma.Context, p access.Principal, publicURL string) bool {
	changes := map[string]bool{http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true}
	if !changes[ctx.Method()] {
		return true
	}
	origin := ctx.Header("Origin")
	if origin == "" {
		return p.Kind() != access.Session
	}
	return AllowedOrigin(origin, ctx.Host(), publicURL)
}

// AllowedOrigin is true for an Origin of the hub: its Host over HTTP or HTTPS,
// or the scheme and host of publicURL.
func AllowedOrigin(origin, host, publicURL string) bool {
	if origin == "" {
		return false
	}
	return origin == "http://"+host || origin == "https://"+host || origin == publicOrigin(publicURL)
}

func publicOrigin(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// Refuse is 401 for nobody and 403 for anyone identified.
func Refuse(ctx context.Context) error {
	if access.FromContext(ctx).Kind() == access.Nobody {
		return huma.ErrorWithHeaders(huma.Error401Unauthorized(msgNobody), Challenge())
	}
	return huma.Error403Forbidden(msgForbidden)
}

// Challenge is the WWW-Authenticate header of a 401.
func Challenge() http.Header {
	h := http.Header{}
	h.Set("WWW-Authenticate", "Bearer")
	return h
}

// statusClientClosed answers a client that left: no one reads it, and it is no server error to log.
const statusClientClosed = 499

// Failure answers an error no route expects: 499 when the client went away, 500 otherwise.
func Failure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return huma.NewError(statusClientClosed, "the client closed the request")
	}
	return huma.Error500InternalServerError("internal error", err)
}

func NeedAdmin(ctx context.Context) error {
	if access.FromContext(ctx).Admin() {
		return nil
	}
	return Refuse(ctx)
}

func Cookie(ctx huma.Context, id string, expires time.Time) http.Cookie {
	return SessionCookie(id, expires, secure(ctx))
}

func ClearCookie(ctx huma.Context) http.Cookie {
	return ClearedSessionCookie(secure(ctx))
}

func SessionCookie(id string, expires time.Time, secure bool) http.Cookie {
	return http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(accounts.SessionLifetime / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	}
}

func ClearedSessionCookie(secure bool) http.Cookie {
	return http.Cookie{
		Name:     CookieName,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	}
}

// secure also trusts X-Forwarded-Proto: behind the proxy the hub only sees HTTP.
func secure(ctx huma.Context) bool {
	return ctx.TLS() != nil || strings.EqualFold(ctx.Header("X-Forwarded-Proto"), "https")
}
