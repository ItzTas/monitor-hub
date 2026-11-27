package tokens

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
		OperationID: "list-tokens",
		Method:      http.MethodGet,
		Path:        "/api/tokens",
		Summary:     "List personal tokens",
		Description: "Your personal tokens, oldest first, without their secrets. With everyone=true, an admin gets every person's tokens.",
		Tags:        []string{"tokens"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden},
	}, h.ListTokens)
	huma.Register(api, huma.Operation{
		OperationID:   "create-token",
		Method:        http.MethodPost,
		Path:          "/api/tokens",
		Summary:       "Create a personal token",
		Description:   "Creates a personal token (mht_). It can carry view, check and hook, each only on monitors where you hold that permission yourself. Only this response shows the token, so copy it now. A request made with it can do only what both the token and you are allowed to do at that moment.",
		Tags:          []string{"tokens"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, h.CreateToken)
	huma.Register(api, huma.Operation{
		OperationID:   "revoke-token",
		Method:        http.MethodDelete,
		Path:          "/api/tokens/{id}",
		Summary:       "Revoke a personal token",
		Description:   "Deletes one of your tokens. It stops working on the next request. An admin can revoke anyone's token. Revoking someone else's gets 404, the same as an id that does not exist.",
		Tags:          []string{"tokens"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}, h.RevokeToken)
	huma.Register(api, huma.Operation{
		OperationID: "list-credentials",
		Method:      http.MethodGet,
		Path:        "/api/credentials",
		Summary:     "List credentials",
		Description: "Your credentials, oldest first, without their passwords. With everyone=true, an admin gets every person's credentials.",
		Tags:        []string{"credentials"},
		Metadata:    gate.Meta(gate.Session),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden},
	}, h.ListCredentials)
	huma.Register(api, huma.Operation{
		OperationID:   "create-credential",
		Method:        http.MethodPost,
		Path:          "/api/credentials",
		Summary:       "Create a credential",
		Description:   "Creates a credential: a username and an mhk_ password for Basic authentication on monitor hooks. It carries only hook, and only on monitors where you hold hook yourself. Only this response shows the password, so copy it now.",
		Tags:          []string{"credentials"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, h.CreateCredential)
	huma.Register(api, huma.Operation{
		OperationID:   "revoke-credential",
		Method:        http.MethodDelete,
		Path:          "/api/credentials/{id}",
		Summary:       "Revoke a credential",
		Description:   "Deletes one of your credentials. It stops working on the next request. An admin can revoke anyone's credential. Revoking someone else's gets 404, the same as an id that does not exist.",
		Tags:          []string{"credentials"},
		Metadata:      gate.Meta(gate.Session),
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}, h.RevokeCredential)
}
