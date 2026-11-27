package session

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/api/gate"
)

func Configure(api huma.API, svc *accounts.Service, after func(time.Duration) <-chan time.Time) {
	RegisterRoutes(api, NewHandler(svc, NewLogins(after, nil)))
}

func RegisterRoutes(api huma.API, h *Handler) {
	huma.Register(api, huma.Operation{
		OperationID: "create-session",
		Method:      http.MethodPost,
		Path:        "/api/session",
		Summary:     "Sign in",
		Description: "Checks the login and password, opens a 12-hour session and sets its cookie. The body is the same as GET /api/me. A wrong login or password is answered after one second, and sign-ins run one at a time.",
		Tags:        []string{"session"},
		Metadata:    gate.Meta(gate.Anyone),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden},
	}, h.Create)
	huma.Register(api, huma.Operation{
		OperationID:   "delete-session",
		Method:        http.MethodDelete,
		Path:          "/api/session",
		Summary:       "Sign out",
		Description:   "Ends the session in the cookie and clears the cookie.",
		Tags:          []string{"session"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden},
	}, h.Delete)
	huma.Register(api, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        "/api/me",
		Summary:     "Show who is signed in",
		Description: "The signed-in person and whether they are an admin. permissions holds only the grants given to the person directly. monitors holds what this session can do on each monitor right now, so an admin has every action even without grants.",
		Tags:        []string{"session"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden},
	}, h.Me)
}
