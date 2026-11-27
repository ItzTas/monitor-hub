package tokens

import "time"

type Token struct {
	ID          int64      `json:"id"`
	Owner       string     `json:"owner" doc:"The login of the person who owns it."`
	Label       string     `json:"label" doc:"The label given when it was created."`
	Permissions []string   `json:"permissions" doc:"What it carries, as permission slugs. A request with it also needs its owner to hold the permission."`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at" doc:"When it stops working, or null if it never expires."`
	LastUsedAt  *time.Time `json:"last_used_at" doc:"When it was last used, updated at most once a minute. Null if it was never used."`
}

type Credential struct {
	Token
	Username string `json:"username" doc:"The username for Basic authentication."`
}

type CreatedToken struct {
	Token
	Secret string `json:"token" doc:"The personal token. This is the only response that shows it."`
}

type CreatedCredential struct {
	Credential
	Password string `json:"password" doc:"The password for Basic authentication. This is the only response that shows it."`
}

type ListInput struct {
	Everyone bool `query:"everyone" doc:"List everyone's, not just your own. Only an admin can."`
}

type ListTokensOutput struct {
	Body struct {
		Items []Token `json:"items"`
	}
}

type ListCredentialsOutput struct {
	Body struct {
		Items []Credential `json:"items"`
	}
}

type CreateTokenInput struct {
	Body struct {
		Label       string     `json:"label" doc:"A name to tell it apart, 1 to 64 characters with no control characters."`
		Permissions []string   `json:"permissions" doc:"Permission slugs, such as monitor:sei:view or monitor:*:check. A token carries view, check and hook, and you must hold each one."`
		ExpiresAt   *time.Time `json:"expires_at,omitempty" doc:"When it stops working. Leave it out and it never expires."`
	}
}

type CreateTokenOutput struct {
	Body CreatedToken
}

type CreateCredentialInput struct {
	Body struct {
		Label       string     `json:"label" doc:"A name to tell it apart, 1 to 64 characters with no control characters."`
		Username    string     `json:"username" doc:"The username for Basic authentication: lowercase letters, digits and single hyphens, 2 to 31 characters, starting with a letter."`
		Permissions []string   `json:"permissions" doc:"Permission slugs for hook, such as monitor:sei:hook or monitor:*:hook. You must hold each one."`
		ExpiresAt   *time.Time `json:"expires_at,omitempty" doc:"When it stops working. Leave it out and it never expires."`
	}
}

type CreateCredentialOutput struct {
	Body CreatedCredential
}

type RevokeInput struct {
	ID int64 `path:"id" doc:"The id from the list."`
}
