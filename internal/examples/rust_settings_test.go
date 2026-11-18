package examples

import (
	"path/filepath"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/plugintest"
)

func TestRustExampleSettings(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "sdk-rust", "crates", "plugin", "examples", "fixed-plugin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range plugintest.CheckSettings(t.Context(), dir, buildRustExample(t, "fixed"), nil) {
		t.Errorf("settings: %s", violation)
	}
}
