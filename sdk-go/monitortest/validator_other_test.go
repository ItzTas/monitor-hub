//go:build !unix

package monitortest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestValidatorNeedsUnix(t *testing.T) {
	dir := t.TempDir()
	settings := `{"settings_version":2,"fields":[{"name":"Events","label":"Events","type":"integer","env":"EVENTS"}]}`
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`settings.json: fields[0]: name "Events" does not match ^[a-z][a-z0-9_]*$`,
		"validate-settings was not run: the validator check needs a Unix system",
	}
	if got := CheckSettings(t.Context(), dir, "monitor", nil); !slices.Equal(got, want) {
		t.Fatalf("violations = %q, want %q", got, want)
	}
}
