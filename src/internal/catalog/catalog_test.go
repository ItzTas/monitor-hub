package catalog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadManifestRejects(t *testing.T) {
	tests := map[string]string{
		"protocol version 1": `{"protocol_version":1,"name":"diario","command":["x"]}`,
		"protocol version 3": `{"protocol_version":3,"name":"diario","command":["x"]}`,
		"uppercase name":     `{"protocol_version":2,"name":"Diario","command":["x"]}`,
		"trailing hyphen":    `{"protocol_version":2,"name":"sei-","command":["x"]}`,
		"double hyphen":      `{"protocol_version":2,"name":"ab--c","command":["x"]}`,
		"short name":         `{"protocol_version":2,"name":"x","command":["x"]}`,
		"empty command":      `{"protocol_version":2,"name":"diario","command":[]}`,
		"unknown field":      `{"protocol_version":2,"name":"diario","command":["x"],"extra":1}`,
		"trailing object":    `{"protocol_version":2,"name":"diario","command":["x"]}{"a":1}`,
		"trailing junk":      `{"protocol_version":2,"name":"diario","command":["x"]} junk`,
		"shell in apk":       `{"protocol_version":2,"name":"diario","command":["x"],"apk":["poppler-utils; rm -rf /"]}`,
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
	raw := `{"protocol_version":2,"name":"diario","command":["diario-alerr","monitor"],"apk":["poppler-utils"]}` + " \n"
	m, err := ReadManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "diario" || !slices.Equal(m.Command, []string{"diario-alerr", "monitor"}) || !slices.Equal(m.Apk, []string{"poppler-utils"}) {
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
	if err := os.WriteFile(filepath.Join(root, dir, "monitor.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRequiresMonitorJSON(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "sei", "")
	_, err := Load(root)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load error = %v, want fs.ErrNotExist", err)
	}
	for _, want := range []string{"monitor directory", "monitor.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "zeta", `{"protocol_version":2,"name":"zeta","command":["z"]}`)
	writeManifest(t, root, "alpha", `{"protocol_version":2,"name":"alpha","command":["a"]}`)
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
			writeManifest(t, root, "foo", `{"protocol_version":2,"name":"bar","command":["x"]}`)
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
			want:    []string{"MONITOR_HUB_DATA_DIR=/data/monitors/diario", "PATH=/bin", "TMPDIR=/tmp", "TZ=America/Boa_Vista", "WATCH_TERMS=a,b"},
		},
		"related monitor names stay isolated": {
			name:    "sei",
			environ: []string{"SEI_EXTRA__TOKEN=a", "SEI__URL=u"},
			want:    []string{"MONITOR_HUB_DATA_DIR=/data/monitors/sei", "TMPDIR=/tmp", "URL=u"},
		},
		"reserved names cannot be overridden": {
			name:    "diario",
			environ: []string{"PATH=/bin", "DIARIO__PATH=/evil", "DIARIO__MONITOR_HUB_DATA_DIR=/x"},
			want:    []string{"MONITOR_HUB_DATA_DIR=/data/monitors/diario", "PATH=/bin", "TMPDIR=/tmp"},
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

func TestValidateNameAcceptsAndRejects(t *testing.T) {
	for name, ok := range map[string]bool{"sei": true, "diario-alerr": true, "a": false, "Sei": false, "sei-": false, "se--i": false, "sei monitor": false, "sei\n": false, "": false} {
		if err := ValidateName(name); (err == nil) != ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
}

func TestManifestValidate(t *testing.T) {
	good := Manifest{ProtocolVersion: 2, Name: "sei", Command: []string{"./sei"}, Apk: []string{"poppler-utils"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"protocol 1": func(m *Manifest) { m.ProtocolVersion = 1 },
		"protocol 3": func(m *Manifest) { m.ProtocolVersion = 3 },
		"name":       func(m *Manifest) { m.Name = "Sei" },
		"command":    func(m *Manifest) { m.Command = nil },
		"apk":        func(m *Manifest) { m.Apk = []string{"curl && id"} },
		"apk option": func(m *Manifest) { m.Apk = []string{"-f"} },
	} {
		m := good
		mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("Validate accepted a manifest with a bad %s", name)
		}
	}
}

func TestManifestExplainsTheRetiredProtocolVersion(t *testing.T) {
	tests := map[string]struct{ raw, want string }{
		"retired": {
			`{"protocol_version":1,"name":"diario","command":["x"]}`,
			"protocol_version 1 is no longer supported: this hub speaks protocol_version 2, where the gRPC service is monitorhub.monitor.v1.Monitor; rebuild the monitor with the current SDK",
		},
		"future": {
			`{"protocol_version":3,"name":"diario","command":["x"]}`,
			"unsupported protocol_version 3: this hub speaks protocol_version 2",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ReadManifest(strings.NewReader(tc.raw))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("ReadManifest error = %v, want %q", err, tc.want)
			}
		})
	}
}
