package examples

import (
	"path/filepath"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/monitortest"
)

func TestRustExampleSettings(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "sdk-rust", "crates", "sdk", "examples", "fixed-monitor"))
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range monitortest.CheckSettings(t.Context(), dir, buildRustExample(t, "fixed"), nil) {
		t.Errorf("settings: %s", violation)
	}
}
