//go:build !unix

package plugintest

import (
	"context"
	"io"
)

const validatorNeedsUnix = validateArg + " was not run: the validator check needs a Unix system"

func validatorViolation(context.Context, string, string, map[string]bool, io.Writer) string {
	return validatorNeedsUnix
}
