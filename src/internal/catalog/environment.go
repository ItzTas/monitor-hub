package catalog

import (
	"slices"
	"sort"
	"strings"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

func EnvPrefix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "__"
}

// Layer changes what a monitor receives from its prefixed variables: Drop removes names, Add
// sets names on top. A name settings.Reserved refuses never passes, and the hub's own names win
// over both.
type Layer struct {
	Drop []string
	Add  map[string]string
}

func Environment(name string, environ []string, dataRoot string, layer Layer) []string {
	prefix := EnvPrefix(name)
	vars := map[string]string{}
	inherited := map[string]string{}
	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		stripped, found := strings.CutPrefix(key, prefix)
		if !found || stripped == "" {
			inherited[key] = value
			continue
		}
		if settings.Reserved(stripped) || slices.Contains(layer.Drop, stripped) {
			continue
		}
		vars[stripped] = value
	}
	for key, value := range layer.Add {
		if !settings.Reserved(key) {
			vars[key] = value
		}
	}
	for _, key := range []string{"PATH", "TZ"} {
		delete(vars, key)
		if value, ok := inherited[key]; ok {
			vars[key] = value
		}
	}
	vars["TMPDIR"] = "/tmp"
	vars["MONITOR_HUB_DATA_DIR"] = dataRoot + "/monitors/" + name

	out := make([]string, 0, len(vars))
	for key, value := range vars {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}
