package catalog

import (
	"sort"
	"strings"
)

func EnvPrefix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "__"
}

func Environment(name string, environ []string, dataRoot string) []string {
	prefix := EnvPrefix(name)
	vars := map[string]string{}
	inherited := map[string]string{}
	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if stripped, found := strings.CutPrefix(key, prefix); found && stripped != "" {
			vars[stripped] = value
			continue
		}
		inherited[key] = value
	}
	for _, key := range []string{"PATH", "TZ"} {
		delete(vars, key)
		if value, ok := inherited[key]; ok {
			vars[key] = value
		}
	}
	vars["TMPDIR"] = "/tmp"
	vars["MONITOR_HUB_DATA_DIR"] = dataRoot + "/plugins/" + name

	out := make([]string, 0, len(vars))
	for key, value := range vars {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}
