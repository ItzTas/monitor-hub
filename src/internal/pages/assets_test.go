package pages

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const htmxSHA256 = "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717"

func TestVendoredHtmxIsTheRecordedRelease(t *testing.T) {
	script, err := static.ReadFile("static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := static.ReadFile("static/htmx.min.js.sha256")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(script)
	got := hex.EncodeToString(sum[:])
	fields := strings.Fields(string(recorded))
	if len(fields) != 2 || fields[1] != "htmx.min.js" {
		t.Fatalf("htmx.min.js.sha256 = %q, want a hash and the name htmx.min.js", recorded)
	}
	if got != fields[0] || got != htmxSHA256 {
		t.Fatalf("htmx.min.js SHA-256 = %s, recorded %s, want %s", got, fields[0], htmxSHA256)
	}
	if !bytes.Contains(script, []byte(`version:"2.0.11"`)) {
		t.Fatal(`htmx.min.js does not contain version:"2.0.11"`)
	}
}

func TestAssetsServeOnlyScriptsAndStyles(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.url("htmx.min.js"), "/static/htmx.min.js?v="+htmxSHA256[:16]; got != want {
		t.Fatalf("url(htmx.min.js) = %q, want %q", got, want)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/{file}", a)
	script, err := static.ReadFile("static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, target, contentType, cacheControl string
		status                                  int
		body                                    []byte
	}{
		{"script with its version", a.url("htmx.min.js"), "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", http.StatusOK, script},
		{"style without a version", "/static/style.css", "text/css; charset=utf-8", "", http.StatusOK, nil},
		{"script with another version", "/static/htmx.min.js?v=0000000000000000", "text/javascript; charset=utf-8", "", http.StatusOK, script},
		{"checksum file", "/static/htmx.min.js.sha256", "", "", http.StatusNotFound, nil},
		{"unknown file", "/static/missing.js", "", "", http.StatusNotFound, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, c.target, nil))
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d", rec.Code, c.status)
			}
			if c.status != http.StatusOK {
				return
			}
			if got := rec.Header().Get("Content-Type"); got != c.contentType {
				t.Errorf("Content-Type = %q, want %q", got, c.contentType)
			}
			if got := rec.Header().Get("Cache-Control"); got != c.cacheControl {
				t.Errorf("Cache-Control = %q, want %q", got, c.cacheControl)
			}
			if c.body != nil && !bytes.Equal(rec.Body.Bytes(), c.body) {
				t.Error("body differs from the embedded file")
			}
		})
	}
}
