//go:build unix

package monitortest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidatorStdoutIsReadUpToItsLimit(t *testing.T) {
	dir := t.TempDir()
	script := "i=0\nwhile [ $i -lt 4000 ]; do printf '%050d' 0; i=$((i+1)); done\n"
	if err := os.WriteFile(filepath.Join(dir, validateArg), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	state, stdout, violation := runValidator(t.Context(), dir, "/bin/sh", nil)
	if violation != "" {
		t.Fatalf("violation = %q, want none", violation)
	}
	if !state.Success() {
		t.Fatalf("the validator ended with %s, want a clean exit after writing everything", state)
	}
	if len(stdout) != answerLimit+1 {
		t.Fatalf("kept %d bytes of stdout, want %d", len(stdout), answerLimit+1)
	}
}
