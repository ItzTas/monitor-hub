package pages

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

func withSession(r *http.Request, id string) *http.Request {
	r.AddCookie(&http.Cookie{Name: gate.CookieName, Value: id})
	return r
}

func TestThePrincipalComesOnlyFromTheSessionCookie(t *testing.T) {
	fake := newFakeAccounts()
	fake.sessions["good"] = access.NewSession(access.Person{Login: "paulo-santos"})

	p, err := principal(withSession(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil), "good"), fake)
	if err != nil || p.Kind() != access.Session || p.Login() != "paulo-santos" {
		t.Errorf("principal with the cookie good = %v %q, %v; want the session of paulo-santos", p.Kind(), p.Login(), err)
	}

	tokenOnly := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil)
	tokenOnly.Header.Set("Authorization", "Bearer mht_x")
	before := fake.callCount("BySession")
	p, err = principal(tokenOnly, fake)
	if err != nil || p.Kind() != access.Nobody || fake.callCount("BySession") != before {
		t.Errorf("principal with only a bearer token = %v, %v after %d calls; want nobody, nil and no call", p.Kind(), err, fake.callCount("BySession")-before)
	}

	p, err = principal(withSession(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil), "gone"), fake)
	if err != nil || p.Kind() != access.Nobody {
		t.Errorf("principal with the cookie gone = %v, %v; want nobody and nil", p.Kind(), err)
	}

	broken := newFakeAccounts()
	broken.sessionErr = errors.New("database down")
	if _, err := principal(withSession(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil), "broken"), broken); !errors.Is(err, broken.sessionErr) {
		t.Errorf("principal with the database down = %v, want the database error", err)
	}
}

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name      string
		origin    string
		publicURL string
		want      bool
	}{
		{"the host of the request", "http://example.com", "", true},
		{"no Origin", "", "", false},
		{"another site", "http://evil.example", "", false},
		{"PUBLIC_URL", "http://hub.lan", "http://hub.lan", true},
	}
	for _, tt := range tests {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/settings/login", nil)
		if tt.origin != "" {
			r.Header.Set("Origin", tt.origin)
		}
		if got := sameOrigin(r, tt.publicURL); got != tt.want {
			t.Errorf("%s: sameOrigin = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsHTTPS(t *testing.T) {
	overTLS := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil)
	overTLS.TLS = &tls.ConnectionState{}
	if !isHTTPS(overTLS) {
		t.Error("isHTTPS over TLS = false, want true")
	}
	tests := map[string]bool{"https": true, "HTTPS": true, "HTTPS, http": false, "http": false, "": false}
	for proto, want := range tests {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil)
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		if got := isHTTPS(r); got != want {
			t.Errorf("isHTTPS with X-Forwarded-Proto %q = %v, want %v", proto, got, want)
		}
	}
}

func TestIsHTMX(t *testing.T) {
	tests := map[string]bool{"true": true, "": false, "false": false}
	for value, want := range tests {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil)
		if value != "" {
			r.Header.Set("HX-Request", value)
		}
		if got := isHTMX(r); got != want {
			t.Errorf("isHTMX with HX-Request %q = %v, want %v", value, got, want)
		}
	}
}

func TestSecureHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	rec := httptest.NewRecorder()
	secureHeaders(next).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil))

	want := map[string]string{
		"Content-Security-Policy":    "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "same-origin",
		"Cross-Origin-Opener-Policy": "same-origin",
	}
	for name, value := range want {
		if got := rec.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want the status of next", rec.Code)
	}
}
