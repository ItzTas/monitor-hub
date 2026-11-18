package icons

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var imagePath = regexp.MustCompile(`^/icons/[0-9a-f]{64}\.png$`)

func TestTheDefaultIconIsTheBell(t *testing.T) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(defaultPNG))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if format != "png" || cfg.Width != 256 || cfg.Height != 256 {
		t.Fatalf("got %s %dx%d, want png 256x256", format, cfg.Width, cfg.Height)
	}
	if len(defaultPNG) > 65536 {
		t.Fatalf("icon is %d bytes, want at most 65536", len(defaultPNG))
	}
	if _, err := png.Decode(bytes.NewReader(defaultPNG)); err != nil {
		t.Fatalf("full decode: %v", err)
	}
	icon := Default()
	if icon.Emoji != "🔔" {
		t.Fatalf("emoji = %q", icon.Emoji)
	}
	if !imagePath.MatchString(icon.Image) {
		t.Fatalf("image = %q", icon.Image)
	}
}

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

func TestTheDefaultIconIsServedByItsHash(t *testing.T) {
	path := Default().Image
	rec := get(t, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	want := map[string]string{
		"Content-Type":            "image/png",
		"Cache-Control":           "public, max-age=31536000, immutable",
		"Content-Security-Policy": "default-src 'none'; sandbox",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	sum := sha256.Sum256(rec.Body.Bytes())
	if got := strings.TrimSuffix(strings.TrimPrefix(path, "/icons/"), ".png"); hex.EncodeToString(sum[:]) != got {
		t.Fatalf("body hash %x does not match path %s", sum, got)
	}
}

func TestOtherIconPathsAreNotFound(t *testing.T) {
	hash := strings.TrimSuffix(strings.TrimPrefix(Default().Image, "/icons/"), ".png")
	paths := []string{
		"/icons/" + strings.Repeat("0", 64) + ".png",
		"/icons/" + strings.ToUpper(hash) + ".png",
		"/icons/" + hash,
		"/icons/" + hash[:63] + ".png",
		"/icons/%2e%2e%2fdefault.png",
	}
	for _, p := range paths {
		if rec := get(t, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", p, rec.Code)
		}
	}
}
