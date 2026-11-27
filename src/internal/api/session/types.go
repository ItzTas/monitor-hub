package session

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ItzTas/monitor-hub/src/internal/accounts"
)

// request keeps the huma context for the cookie, which depends on how the request came in.
type request struct {
	ctx huma.Context
}

func (r *request) Resolve(ctx huma.Context) []error {
	r.ctx = ctx
	return nil
}

type CreateInput struct {
	request
	Body struct {
		Login    string `json:"login" doc:"The person's login."`
		Password string `json:"password" doc:"The person's password."`
	}
}

type CreateOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      accounts.Me
}

type DeleteInput struct {
	request
	Session string `cookie:"monitor_hub_session" hidden:"true"`
}

type DeleteOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
}

type MeOutput struct {
	Body accounts.Me
}
