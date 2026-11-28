// Package monitorsettings serves each monitor's configuration to sessions with the configure permission.
package monitorsettings

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

// BodyBytes is the largest body a configuration save takes.
const BodyBytes = 128 << 10

const (
	readPermission   = " To read a monitor's configuration you need a signed-in session and the configure permission on that monitor. Without it you get 403, even for a name no monitor has."
	changePermission = " To change a monitor's configuration you need a signed-in session and the configure permission on that monitor. Without it you get 403, even for a name no monitor has."
)

func Configure(api huma.API, service Service) {
	RegisterRoutes(api, NewHandler(service))
}

func RegisterRoutes(api huma.API, h *Handler) {
	refused := []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}
	huma.Register(api, huma.Operation{
		OperationID: "get-monitor-settings",
		Method:      http.MethodGet,
		Path:        "/api/monitors/{name}/settings",
		Summary:     "Read a monitor's configuration",
		Description: "The monitor's declaration, the values it receives and where each one comes from, the saved values it no longer takes, its free variables and the problems in its .env text. It never shows a secret value, only whether one is set." + readPermission,
		Tags:        []string{"settings"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      append(refused, http.StatusServiceUnavailable),
	}, h.Read)
	huma.Register(api, huma.Operation{
		OperationID:   "put-monitor-settings",
		Method:        http.MethodPut,
		Path:          "/api/monitors/{name}/settings",
		Summary:       "Change a monitor's configuration",
		Description:   "Merges the body over what is saved: a value saves a field, null sends it back to the .env text or the default, and anything the body leaves out stays as it is. The monitor checks the result first. When what the monitor receives changes, the hub restarts it and answers 202; otherwise it answers 200." + changePermission,
		Tags:          []string{"settings"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusAccepted,
		MaxBodyBytes:  BodyBytes,
		Errors:        append(refused, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadGateway, http.StatusServiceUnavailable),
	}, h.Save)
	huma.Register(api, huma.Operation{
		OperationID: "get-monitor-settings-progress",
		Method:      http.MethodGet,
		Path:        "/api/monitors/{name}/settings/progress",
		Summary:     "Follow the restart of the last save",
		Description: "Shows how the restart asked for by the last save is going. Only saves made since the hub started count." + readPermission,
		Tags:        []string{"settings"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      append(refused, http.StatusServiceUnavailable),
	}, h.Progress)
}
