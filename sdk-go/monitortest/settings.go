package monitortest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

const (
	settingsFile  = settings.File
	validateArg   = settings.ValidateArg
	validateLimit = 5 * time.Second
	answerLimit   = 64 << 10
)

func CheckSettings(ctx context.Context, dir, program string, stderr io.Writer) []string {
	if violation := dirViolation(dir); violation != "" {
		return []string{violation}
	}
	data, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	out := fileViolations(data, err)
	var declaration *settings.Declaration
	if len(out) == 0 {
		// Read refuses only what Check refuses, so it cannot fail here.
		declaration, _ = settings.Read(data)
	}
	if violation := validatorViolation(ctx, dir, program, declaration, stderr); violation != "" {
		out = append(out, violation)
	}
	return out
}

func dirViolation(dir string) string {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Sprintf("%s: cannot read: %v", settingsFile, err)
	}
	if !info.IsDir() {
		return fmt.Sprintf("%s: cannot read: %s is not a directory", settingsFile, dir)
	}
	return ""
}

func fileViolations(data []byte, err error) []string {
	if err != nil {
		return []string{fmt.Sprintf("%s: cannot read: %v", settingsFile, err)}
	}
	return settings.Check(data)
}
