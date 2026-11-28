package people

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

func Configure(api huma.API, svc *accounts.Service) {
	RegisterRoutes(api, NewHandler(svc))
}

func RegisterRoutes(api huma.API, h *Handler) {
	huma.Register(api, huma.Operation{
		OperationID: "list-users",
		Method:      http.MethodGet,
		Path:        "/api/users",
		Summary:     "List people",
		Description: "Every person, sorted by login, with whether they are an admin, whether they are disabled, and the permissions given to them directly. Only an admin can list people.",
		Tags:        []string{"people"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden},
	}, h.List)
	huma.Register(api, huma.Operation{
		OperationID:   "create-user",
		Method:        http.MethodPost,
		Path:          "/api/users",
		Summary:       "Create a person",
		Description:   "Creates a person who signs in with this login and password. Set admin to true to make them an admin. No response ever shows the password. Only an admin can create people.",
		Tags:          []string{"people"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity},
	}, h.Create)
	huma.Register(api, huma.Operation{
		OperationID: "update-user",
		Method:      http.MethodPatch,
		Path:        "/api/users/{login}",
		Summary:     "Change a person",
		Description: "Sets whether the person is an admin, whether they are disabled, or both. A field left out stays as it is, and the two changes apply together or not at all. A disabled person cannot sign in, and their personal tokens stop working on the next request. Disabling a person also ends their sessions, so once they are enabled again their tokens work but they have to sign in again. A change that would leave no active admin gets 409. Only an admin can do this.",
		Tags:        []string{"people"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}, h.Update)
	huma.Register(api, huma.Operation{
		OperationID:   "set-user-password",
		Method:        http.MethodPut,
		Path:          "/api/users/{login}/password",
		Summary:       "Set a person's password",
		Description:   "Replaces the person's password and ends their sessions. Their personal tokens keep working. Only an admin can do this.",
		Tags:          []string{"people"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, h.SetPassword)
	huma.Register(api, huma.Operation{
		OperationID: "set-user-permissions",
		Method:      http.MethodPut,
		Path:        "/api/users/{login}/permissions",
		Summary:     "Set a person's permissions",
		Description: "Replaces the whole list of permissions given to the person directly. An empty list removes them all. A monitor that does not exist gets 404. An admin can already do everything, but the list is kept and applies again if they stop being an admin. Only an admin can do this.",
		Tags:        []string{"people"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, h.SetPermissions)
	huma.Register(api, huma.Operation{
		OperationID:   "delete-user",
		Method:        http.MethodDelete,
		Path:          "/api/users/{login}",
		Summary:       "Delete a person",
		Description:   "Deletes the person along with their sessions, personal tokens and credentials. Deleting the last active admin gets 409. Only an admin can do this.",
		Tags:          []string{"people"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}, h.Delete)
}
