package catalog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

func TestReadManifestRejects(t *testing.T) {
	tests := map[string]string{
		"no protocol version": `{"name":"diario","command":["x"]}`,
		"uppercase name":      `{"protocol_version":2,"name":"Diario","command":["x"]}`,
		"trailing hyphen":     `{"protocol_version":2,"name":"sei-","command":["x"]}`,
		"double hyphen":       `{"protocol_version":2,"name":"ab--c","command":["x"]}`,
		"short name":          `{"protocol_version":2,"name":"x","command":["x"]}`,
		"empty command":       `{"protocol_version":2,"name":"diario","command":[]}`,
		"unknown field":       `{"protocol_version":2,"name":"diario","command":["x"],"extra":1}`,
		"trailing object":     `{"protocol_version":2,"name":"diario","command":["x"]}{"a":1}`,
		"trailing junk":       `{"protocol_version":2,"name":"diario","command":["x"]} junk`,
		"shell in apk":        `{"protocol_version":2,"name":"diario","command":["x"],"apk":["poppler-utils; rm -rf /"]}`,
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
			got := Environment(tt.name, tt.environ, "/data", Layer{})
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnvironmentLayers(t *testing.T) {
	tests := map[string]struct {
		name    string
		environ []string
		layer   Layer
		want    []string
	}{
		"drop, add and the hub's names": {
			name:    "sei",
			environ: []string{"SEI__SEI_URL=u", "SEI__SEI_PASSWORD=p", "SEI__HTTP_PROXY=h", "SEI__MONITOR_HUB_CONFIG_FD=7", "SEI__MONITOR_HUB_X=y", "SEI__PATH=/evil", "PATH=/bin", "TZ=X"},
			layer:   Layer{Drop: []string{"SEI_URL", "SEI_PASSWORD"}, Add: map[string]string{"FREE": "v", "PATH": "/x"}},
			want:    []string{"FREE=v", "HTTP_PROXY=h", "MONITOR_HUB_DATA_DIR=/data/monitors/sei", "PATH=/bin", "TMPDIR=/tmp", "TZ=X"},
		},
		"no layer": {
			name:    "anhanguera",
			environ: []string{"ANHANGUERA__MONITOR_HUB_CONFIG_FD=3", "ANHANGUERA__COURSES=c"},
			want:    []string{"COURSES=c", "MONITOR_HUB_DATA_DIR=/data/monitors/anhanguera", "TMPDIR=/tmp"},
		},
		"added PATH and TZ give way to the hub's even when it has none": {
			name:  "sei",
			layer: Layer{Add: map[string]string{"PATH": "/x", "TZ": "Y"}},
			want:  []string{"MONITOR_HUB_DATA_DIR=/data/monitors/sei", "TMPDIR=/tmp"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := Environment(tt.name, tt.environ, "/data", tt.layer)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnvironmentDropsReservedNames(t *testing.T) {
	want := []string{"MONITOR_HUB_DATA_DIR=/data/monitors/sei", "PATH=/bin", "TMPDIR=/tmp", "TZ=X", "URL=u"}
	for _, reserved := range []string{"SEI__PATH=/evil", "SEI__TZ=Y", "SEI__TMPDIR=/x", "SEI__MONITOR_HUB_CONFIG_FD=3", "SEI__PLUGIN_X=p", "SEI__LD_PRELOAD=/l.so"} {
		t.Run(reserved, func(t *testing.T) {
			got := Environment("sei", []string{"PATH=/bin", "TZ=X", "SEI__URL=u", reserved}, "/data", Layer{})
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestEnvironmentKeepsReservedNamesOutOfTheLayer(t *testing.T) {
	add := map[string]string{
		"FREE": "v", "MONITOR_HUB_CONFIG_FD": "3", "MONITOR_HUB_DATA_DIR": "/x", "PLUGIN_PROTOCOL_VERSIONS": "1", "LD_PRELOAD": "/l.so", "TMPDIR": "/x",
		"PATH=/evil": "x", "TZ=x": "", "": "e", "lower": "l", "NUL\x00X": "n",
	}
	want := []string{"FREE=v", "MONITOR_HUB_DATA_DIR=/data/monitors/sei", "PATH=/bin", "TMPDIR=/tmp"}
	got := Environment("sei", []string{"PATH=/bin"}, "/data", Layer{Add: add})
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func writeSettings(t *testing.T, root, dir string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, dir, settings.File), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// padded fills body with trailing spaces up to size bytes, so only its size can make it unreadable.
func padded(body string, size int) []byte {
	return append([]byte(body), strings.Repeat(" ", size-len(body))...)
}

func TestLoadReadsTheDeclarationBesideTheManifest(t *testing.T) {
	const declaration = `{"settings_version":2,"fields":[{"name":"url","label":"URL","type":"string","env":"URL","later":1}]}`
	root := t.TempDir()
	for _, name := range []string{"aa", "bb", "cc", "dd", "ee", "ff", "gg"} {
		writeManifest(t, root, name, `{"protocol_version":2,"name":"`+name+`","command":["x"]}`)
	}
	writeSettings(t, root, "aa", padded(declaration, 1<<20))
	writeSettings(t, root, "cc", []byte(`{"settings_version":1,"fields":[]}`))
	if err := os.Mkdir(filepath.Join(root, "dd", settings.File), 0o750); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, root, "ee", padded(declaration, 1<<20+1))
	writeSettings(t, root, "ff", []byte(`{"settings_version":2,"fields":[{"name":"p","label":"P","type":"string","Secret":true}]}`))
	if err := os.WriteFile(filepath.Join(root, "outside.json"), []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "outside.json"), filepath.Join(root, "gg", settings.File)); err != nil {
		t.Fatal(err)
	}

	entries, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Manifest.Name] = e
	}

	a := byName["aa"]
	if a.SettingsError != nil || a.Settings == nil || len(a.Settings.Fields) != 1 {
		t.Fatalf("aa: Settings = %+v, SettingsError = %v", a.Settings, a.SettingsError)
	}
	if f := a.Settings.Fields[0]; f.Name != "url" || f.Label != "URL" || f.Type != settings.String || f.Env != "URL" || !f.Editable {
		t.Fatalf("aa: field = %+v", f)
	}
	if b := byName["bb"]; b.Settings != nil || b.SettingsError != nil {
		t.Fatalf("bb: Settings = %+v, SettingsError = %v, want both nil", b.Settings, b.SettingsError)
	}

	wantErrors := map[string]string{
		"cc": "settings.json: settings_version is 1, want 2",
		"ee": "settings.json is larger than 1 MiB",
		"ff": `settings.json: key "Secret" differs from "secret" only in letter case`,
	}
	for name, want := range wantErrors {
		e := byName[name]
		if e.Settings != nil || e.SettingsError == nil || e.SettingsError.Error() != want {
			t.Errorf("%s: Settings = %+v, SettingsError = %v, want nil and %q", name, e.Settings, e.SettingsError, want)
		}
	}
	if d, want := byName["dd"], settings.File+" is not a regular file"; d.Settings != nil || d.SettingsError == nil || d.SettingsError.Error() != want {
		t.Errorf("dd: Settings = %+v, SettingsError = %v, want nil and %q", d.Settings, d.SettingsError, want)
	}
	if g := byName["gg"]; g.Settings != nil || g.SettingsError == nil || !strings.Contains(g.SettingsError.Error(), "path escapes from parent") {
		t.Errorf("gg: Settings = %+v, SettingsError = %v, want nil and an escape refused", g.Settings, g.SettingsError)
	}
}

func TestLoadRefusesASettingsFileThatIsAFIFO(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "fifo", `{"protocol_version":2,"name":"fifo","command":["x"]}`)
	if err := syscall.Mkfifo(filepath.Join(root, "fifo", settings.File), 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		entries []Entry
		err     error
	}
	done := make(chan result, 1)
	go func() {
		entries, err := Load(root)
		done <- result{entries, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Load did not return within 10s: it waits for a writer on the FIFO")
	}
	if got.err != nil || len(got.entries) != 1 {
		t.Fatalf("Load = %d entries, %v; want one entry", len(got.entries), got.err)
	}
	want := settings.File + " is not a regular file"
	if e := got.entries[0]; e.Settings != nil || e.SettingsError == nil || e.SettingsError.Error() != want {
		t.Errorf("Settings = %+v, SettingsError = %v, want nil and %q", e.Settings, e.SettingsError, want)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCommand(t *testing.T) {
	dir := t.TempDir()
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "tool"))
	writeExecutable(t, filepath.Join(pathDir, "tool"))
	writeExecutable(t, filepath.Join(pathDir, "other"))
	writeExecutable(t, filepath.Join(filepath.Dir(dir), "escape"))
	env := []string{"PATH=" + pathDir}
	probe := func(command string) Entry {
		return Entry{Manifest: Manifest{ProtocolVersion: 2, Name: "probe", Command: []string{command}}, Dir: dir}
	}

	found := map[string]struct{ command, want string }{
		"file in the directory wins": {command: "tool", want: filepath.Join(dir, "tool")},
		"falls back to PATH":         {command: "other", want: filepath.Join(pathDir, "other")},
	}
	for name, tt := range found {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveCommand(probe(tt.command), env)
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	for name, command := range map[string]string{"unknown command": "missing", "escape through ..": "../escape"} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveCommand(probe(command), env)
			if err == nil || !strings.Contains(err.Error(), "probe") {
				t.Fatalf("got error %v, want one naming the monitor", err)
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

func TestManifestNamesTheProtocolVersionItRefuses(t *testing.T) {
	_, err := ReadManifest(strings.NewReader(`{"protocol_version":3,"name":"diario","command":["x"]}`))
	want := "unsupported protocol_version 3: this hub speaks protocol_version 2"
	if err == nil || err.Error() != want {
		t.Fatalf("ReadManifest error = %v, want %q", err, want)
	}
}
