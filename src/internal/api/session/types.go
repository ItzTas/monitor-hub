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
	Body Credentials
}

// Credentials takes missing and unknown fields: huma's error for them would echo the body, password included.
type Credentials struct {
	_        struct{} `additionalProperties:"true"`
	Login    string   `json:"login" required:"false" doc:"The person's login."`
	Password string   `json:"password" required:"false" doc:"The person's password."`
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
