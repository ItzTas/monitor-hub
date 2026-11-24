//go:build !unix

package monitortest

import (
	"context"
	"io"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

const validatorNeedsUnix = validateArg + " was not run: the validator check needs a Unix system"

func validatorViolation(context.Context, string, string, *settings.Declaration, io.Writer) string {
	return validatorNeedsUnix
}
