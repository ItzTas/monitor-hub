package pages

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
)

//go:embed static
var static embed.FS

type asset struct {
	version     string
	contentType string
	body        []byte
}

// assets serves the scripts and styles of static under /static/{file}.
type assets struct {
	files map[string]asset
}

func loadAssets() (assets, error) {
	types := map[string]string{
		".js":  "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8",
	}
	entries, err := fs.ReadDir(static, "static")
	if err != nil {
		return assets{}, err
	}
	a := assets{files: map[string]asset{}}
	for _, entry := range entries {
		contentType, ok := types[path.Ext(entry.Name())]
		if !ok || entry.IsDir() {
			continue
		}
		body, err := static.ReadFile("static/" + entry.Name())
		if err != nil {
			return assets{}, err
		}
		sum := sha256.Sum256(body)
		a.files[entry.Name()] = asset{version: hex.EncodeToString(sum[:8]), contentType: contentType, body: body}
	}
	return a, nil
}

func (a assets) url(file string) string {
	return "/static/" + file + "?v=" + a.files[file].version
}

func (a assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	file, ok := a.files[r.PathValue("file")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Only a URL that names this exact version may be cached for good.
	if r.URL.Query().Get("v") == file.version {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Header().Set("Content-Type", file.contentType)
	_, _ = w.Write(file.body)
}
