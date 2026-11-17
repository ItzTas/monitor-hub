//go:build !unix

package plugintest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestValidatorNeedsUnix(t *testing.T) {
	dir := t.TempDir()
	settings := `{"settings_version":1,"fields":[{"name":"events","label":"Events","type":"integer"}]}`
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`settings.json: fields[0]: name "events" does not match ^[A-Z][A-Z0-9_]*$`,
		"validate-settings was not run: the validator check needs a Unix system",
	}
	if got := CheckSettings(t.Context(), dir, "plugin", nil); !slices.Equal(got, want) {
		t.Fatalf("violations = %q, want %q", got, want)
	}
}
