package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ResolveCommand(entry Entry, env []string) (string, error) {
	command := entry.Manifest.Command[0]
	local := filepath.Join(entry.Dir, command)
	if filepath.IsLocal(command) && isExecutable(local) {
		return local, nil
	}
	if path, ok := lookPath(command, EnvValue(env, "PATH")); ok {
		return path, nil
	}
	return "", fmt.Errorf("monitor %s: command %q not found", entry.Manifest.Name, command)
}

func lookPath(command, pathList string) (string, bool) {
	if filepath.IsAbs(command) {
		return command, isExecutable(command)
	}
	if strings.ContainsRune(command, filepath.Separator) {
		return "", false
	}
	for _, dir := range filepath.SplitList(pathList) {
		candidate := filepath.Join(dir, command)
		if filepath.IsAbs(dir) && isExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func EnvValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
