package core

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

type reader struct {
	env      map[string]string
	readFile func(string) ([]byte, error)
	problems []string
}

func (r *reader) problem(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

func (r *reader) err() error {
	if len(r.problems) == 0 {
		return nil
	}
	return errors.New("invalid configuration:\n  - " + strings.Join(r.problems, "\n  - "))
}

func (r *reader) text(name string) string {
	return strings.TrimSpace(r.env[name])
}

func (r *reader) required(name, hint string) string {
	value := r.text(name)
	if value == "" {
		r.problem("%s is required%s", name, hint)
	}
	return value
}

func (r *reader) secret(name string) Secret {
	direct := r.env[name]
	if strings.TrimSpace(direct) == "" {
		direct = ""
	}
	path := r.text(name + "_FILE")
	if direct != "" && path != "" {
		r.problem("set either %s or %s_FILE, not both", name, name)
		return Secret{}
	}
	if path == "" {
		return NewSecret(direct)
	}
	return r.secretFile(name, path)
}

func (r *reader) secretFile(name, path string) Secret {
	content, err := r.readFile(path)
	if err != nil {
		r.problem("%s_FILE: %s", name, describeFileError(path, err))
		return Secret{}
	}
	value := trimFinalNewline(string(content))
	if value == "" {
		r.problem("%s_FILE: %s is empty", name, path)
	}
	return NewSecret(value)
}

func describeFileError(path string, err error) string {
	reasons := []struct {
		target error
		format string
	}{
		{fs.ErrNotExist, "%s does not exist"},
		{fs.ErrPermission, "no permission to read %s"},
	}
	for _, reason := range reasons {
		if errors.Is(err, reason.target) {
			return fmt.Sprintf(reason.format, path)
		}
	}
	return fmt.Sprintf("could not read %s: %v", path, err)
}

func trimFinalNewline(text string) string {
	if trimmed, ok := strings.CutSuffix(text, "\r\n"); ok {
		return trimmed
	}
	return strings.TrimSuffix(text, "\n")
}

func (r *reader) integer(name string, fallback, low, high int) int {
	value := r.text(name)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < low || n > high {
		r.problem("%s: use a whole number from %d to %d (got %q)", name, low, high, value)
		return fallback
	}
	return n
}
