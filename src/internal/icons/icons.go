package icons

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strconv"
)

//go:embed default.png
var defaultPNG []byte

type Icon struct {
	Emoji string `json:"emoji"`
	Image string `json:"image"`
}

var (
	defaultHash = func() string {
		sum := sha256.Sum256(defaultPNG)
		return hex.EncodeToString(sum[:])
	}()
	defaultIcon = Icon{Emoji: "🔔", Image: "/icons/" + defaultHash + ".png"}
)

func Default() Icon {
	return defaultIcon
}

func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /icons/{file}", serve)
}

func serve(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("file") != defaultHash+".png" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Length", strconv.Itoa(len(defaultPNG)))
	_, _ = w.Write(defaultPNG)
}
