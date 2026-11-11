package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadManifestRejects(t *testing.T) {
	tests := map[string]string{
		"protocol version 2": `{"protocol_version":2,"name":"diario","command":["x"]}`,
		"uppercase name":     `{"protocol_version":1,"name":"Diario","command":["x"]}`,
		"trailing hyphen":    `{"protocol_version":1,"name":"sei-","command":["x"]}`,
		"double hyphen":      `{"protocol_version":1,"name":"ab--c","command":["x"]}`,
		"short name":         `{"protocol_version":1,"name":"x","command":["x"]}`,
		"empty command":      `{"protocol_version":1,"name":"diario","command":[]}`,
		"unknown field":      `{"protocol_version":1,"name":"diario","command":["x"],"extra":1}`,
		"trailing object":    `{"protocol_version":1,"name":"diario","command":["x"]}{"a":1}`,
		"trailing junk":      `{"protocol_version":1,"name":"diario","command":["x"]} junk`,
		"shell in apk":       `{"protocol_version":1,"name":"diario","command":["x"],"apk":["poppler-utils; rm -rf /"]}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadManifest(strings.NewReader(raw)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestReadManifestAccepts(t *testing.T) {
	raw := `{"protocol_version":1,"name":"diario","command":["diario-alerr","plugin"],"apk":["poppler-utils"]}` + " \n"
	m, err := ReadManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "diario" || !slices.Equal(m.Command, []string{"diario-alerr", "plugin"}) || !slices.Equal(m.Apk, []string{"poppler-utils"}) {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

func writeManifest(t *testing.T, root, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(root, dir, "plugin.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "zeta", `{"protocol_version":1,"name":"zeta","command":["z"]}`)
	writeManifest(t, root, "alpha", `{"protocol_version":1,"name":"alpha","command":["a"]}`)
	entries, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Manifest.Name != "alpha" || entries[1].Manifest.Name != "zeta" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if entries[0].Dir != filepath.Join(root, "alpha") {
		t.Fatalf("unexpected dir: %s", entries[0].Dir)
	}
}

func TestLoadEmptyRoot(t *testing.T) {
	entries, err := Load(t.TempDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("got %v, %v", entries, err)
	}
}

func TestLoadFails(t *testing.T) {
	tests := map[string]func(t *testing.T) string{
		"name differs from directory": func(t *testing.T) string {
			root := t.TempDir()
			writeManifest(t, root, "foo", `{"protocol_version":1,"name":"bar","command":["x"]}`)
			return root
		},
		"directory without manifest": func(t *testing.T) string {
			root := t.TempDir()
			writeManifest(t, root, "foo", "")
			return root
		},
		"missing root": func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "absent")
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(setup(t)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestEnvPrefix(t *testing.T) {
	tests := map[string]string{
		"diario":       "DIARIO__",
		"diario-alerr": "DIARIO_ALERR__",
	}
	for name, want := range tests {
		if got := EnvPrefix(name); got != want {
			t.Errorf("EnvPrefix(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestEnvironment(t *testing.T) {
	tests := map[string]struct {
		name    string
		environ []string
		want    []string
	}{
		"prefixed and reserved": {
			name:    "diario",
			environ: []string{"DIARIO__WATCH_TERMS=a,b", "TELEGRAM_TOKEN=s", "SEI__SEI_URL=u", "PATH=/bin", "TZ=America/Boa_Vista"},
			want:    []string{"MONITOR_HUB_DATA_DIR=/data/plugins/diario", "PATH=/bin", "TMPDIR=/tmp", "TZ=America/Boa_Vista", "WATCH_TERMS=a,b"},
		},
		"related plugin names stay isolated": {
			name:    "sei",
			environ: []string{"SEI_EXTRA__TOKEN=a", "SEI__URL=u"},
			want:    []string{"MONITOR_HUB_DATA_DIR=/data/plugins/sei", "TMPDIR=/tmp", "URL=u"},
		},
		"reserved names cannot be overridden": {
			name:    "diario",
			environ: []string{"PATH=/bin", "DIARIO__PATH=/evil", "DIARIO__MONITOR_HUB_DATA_DIR=/x"},
			want:    []string{"MONITOR_HUB_DATA_DIR=/data/plugins/diario", "PATH=/bin", "TMPDIR=/tmp"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := Environment(tt.name, tt.environ, "/data")
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
