package access

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

type Action string

const (
	View      Action = "view"
	Check     Action = "check"
	Hook      Action = "hook"
	Configure Action = "configure"
)

// Every in place of a monitor name means all monitors.
const Every = "*"

type Permission struct {
	Monitor string
	Action  Action
}

var ErrInvalid = errors.New("not a permission")

func Actions() []Action {
	return []Action{View, Check, Hook, Configure}
}

// Parse reads a slug written as monitor:<name>:<action>.
func Parse(slug string) (Permission, error) {
	invalid := fmt.Errorf("%w: %q", ErrInvalid, slug)
	parts := strings.Split(slug, ":")
	if len(parts) != 3 || parts[0] != "monitor" {
		return Permission{}, invalid
	}
	actions := map[string]Action{"view": View, "check": Check, "hook": Hook, "configure": Configure}
	action, ok := actions[parts[2]]
	if !ok {
		return Permission{}, invalid
	}
	monitor := parts[1]
	if monitor != Every && catalog.ValidateName(monitor) != nil {
		return Permission{}, invalid
	}
	return Permission{Monitor: monitor, Action: action}, nil
}

func (p Permission) String() string {
	return "monitor:" + p.Monitor + ":" + string(p.Action)
}

// grants reports whether the list holds a on monitor, directly or through Every.
func grants(list []Permission, a Action, monitor string) bool {
	for _, p := range list {
		if p.Action == a && (p.Monitor == monitor || p.Monitor == Every) {
			return true
		}
	}
	return false
}
