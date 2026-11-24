package monitortest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCheckSettingsReportsTheFileViolations(t *testing.T) {
	tests := map[string]struct {
		file string
		want []string
	}{
		"two broken fields": {
			`{"settings_version":2,"fields":[{"name":"Events","label":"Events","type":"integer"},{"name":"mode","label":" ","type":"string"}]}`,
			[]string{
				`settings.json: fields[0]: name "Events" does not match ^[a-z][a-z0-9_]*$`,
				"settings.json: fields[1]: label is empty",
			},
		},
		"byte order mark": {
			"\xef\xbb\xbf" + `{"settings_version":2,"fields":[]}`,
			[]string{"settings.json starts with a UTF-8 byte order mark"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(tt.file), 0o600); err != nil {
				t.Fatal(err)
			}
			got := CheckSettings(t.Context(), dir, "/nonexistent/monitor", nil)
			// The validator's own line comes last.
			n := len(tt.want)
			if len(got) != n+1 || !slices.Equal(got[:n], tt.want) || !strings.HasPrefix(got[n], validateArg+" ") {
				t.Fatalf("violations = %q, want %q, then the validator's line", got, tt.want)
			}
		})
	}
}

func TestCheckSettingsDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := map[string]string{
		"missing directory":   filepath.Join(t.TempDir(), "missing"),
		"empty path":          "",
		"file, not directory": file,
	}
	for name, dir := range unreadable {
		t.Run(name, func(t *testing.T) {
			got := CheckSettings(t.Context(), dir, "/nonexistent/monitor", nil)
			if len(got) != 1 || !strings.HasPrefix(got[0], "settings.json: cannot read: ") {
				t.Fatalf("violations = %q, want one starting with %q", got, "settings.json: cannot read: ")
			}
		})
	}
	t.Run("directory without settings.json", func(t *testing.T) {
		if got := CheckSettings(t.Context(), t.TempDir(), "/nonexistent/monitor", nil); got != nil {
			t.Fatalf("violations = %q, want none", got)
		}
	})
}
