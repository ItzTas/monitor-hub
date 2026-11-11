package health

import (
	"os"
	"testing"

	"github.com/ItzTas/monitor-hub/internal/core/coretest"
)

func TestMain(m *testing.M) {
	os.Exit(coretest.Run(m))
}
