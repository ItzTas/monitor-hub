package api

import (
	"os"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/core/coretest"
)

func TestMain(m *testing.M) {
	// The database driver gives times in the local zone, and the hub runs outside UTC (compose sets TZ).
	time.Local = time.FixedZone("UTC-4", -4*60*60)
	os.Exit(coretest.Run(m))
}
