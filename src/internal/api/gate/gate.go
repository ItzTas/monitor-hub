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
	publicOrigin := ""
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" {
		publicOrigin = u.Scheme + "://" + u.Host
	}
	return func(ctx huma.Context, next func(huma.Context)) {
		level := Session
		if l, ok := ctx.Operation().Metadata["gate"].(Level); ok {
			level = l
		}
		p, refused, err := identify(ctx, resolver, level)
		if err != nil {
			_ = huma.WriteErr(api, ctx, http.StatusInternalServerError, "internal error", err)
			return
		}
		if refused == nil {
			refused = check(ctx, p, level, publicOrigin)
		}
		if refused == nil {
			next(huma.WithContext(ctx, access.WithPrincipal(ctx.Context(), p)))
			return
		}
		if refused.status == http.StatusUnauthorized {
			ctx.SetHeader("WWW-Authenticate", "Bearer")
		}
		_ = huma.WriteErr(api, ctx, refused.status, refused.msg)
	}
}

// identify reads the Authorization header, or else the session cookie. A
// credential that does not hold is refused and never taken for nobody, except
// a stale cookie on an Anyone route, so that it cannot block signing in.
func identify(ctx huma.Context, resolver Resolver, level Level) (access.Principal, *refusal, error) {
	invalid := &refusal{http.StatusUnauthorized, accounts.ErrUnauthenticated.Error()}
	if auth := ctx.Header("Authorization"); auth != "" {
		secret, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
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

func check(ctx huma.Context, p access.Principal, level Level, publicOrigin string) *refusal {
	if p.Kind() == access.Nobody && level != Anyone {
		return &refusal{http.StatusUnauthorized, msgNobody}
	}
	if p.Kind() != access.Session && level == Session {
		return &refusal{http.StatusForbidden, msgNeedSession}
	}
	if !fromThisSite(ctx, p, publicOrigin) {
		return &refusal{http.StatusForbidden, msgOrigin}
	}
	return nil
}

// fromThisSite asks a change for an Origin of the hub itself, and a session for an
// Origin at all: browsers send one, so a session change without it is suspect.
func fromThisSite(ctx huma.Context, p access.Principal, publicOrigin string) bool {
	changes := map[string]bool{http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true}
	if !changes[ctx.Method()] {
		return true
	}
	origin := ctx.Header("Origin")
	if origin == "" {
		return p.Kind() != access.Session
	}
	return origin == "http://"+ctx.Host() || origin == "https://"+ctx.Host() || origin == publicOrigin
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

func NeedAdmin(ctx context.Context) error {
	if access.FromContext(ctx).Admin() {
		return nil
	}
	return Refuse(ctx)
}

func Cookie(ctx huma.Context, id string, expires time.Time) http.Cookie {
	return http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(accounts.SessionLifetime / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure(ctx),
	}
}

func ClearCookie(ctx huma.Context) http.Cookie {
	return http.Cookie{
		Name:     CookieName,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure(ctx),
	}
}

// secure also trusts X-Forwarded-Proto: behind the proxy the hub only sees HTTP.
func secure(ctx huma.Context) bool {
	return ctx.TLS() != nil || strings.EqualFold(ctx.Header("X-Forwarded-Proto"), "https")
}
