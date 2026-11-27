package people

import "time"

type User struct {
	Login       string    `json:"login"`
	Admin       bool      `json:"admin" doc:"An admin can do everything."`
	Disabled    bool      `json:"disabled" doc:"A disabled person cannot sign in, and their sessions and tokens do not work."`
	CreatedAt   time.Time `json:"created_at"`
	Permissions []string  `json:"permissions" doc:"The permissions given to the person directly, as slugs."`
}

type ListOutput struct {
	Body struct {
		Items []User `json:"items"`
	}
}

type UserOutput struct {
	Body User
}

type CreateInput struct {
	Body struct {
		Login    string `json:"login" doc:"Lowercase letters, digits and single hyphens, 2 to 31 characters, starting with a letter."`
		Password string `json:"password" doc:"At least 12 characters and at most 256 bytes."`
		Admin    bool   `json:"admin,omitempty" doc:"Whether the person is an admin. Defaults to false."`
	}
}

type UpdateInput struct {
	Login string `path:"login" doc:"The person's login."`
	Body  struct {
		Admin    *bool `json:"admin,omitempty" doc:"Make the person an admin, or stop them being one. Leave it out to keep it as it is."`
		Disabled *bool `json:"disabled,omitempty" doc:"Disable the person, or enable them again. Leave it out to keep it as it is."`
	}
}

type PasswordInput struct {
	Login string `path:"login" doc:"The person's login."`
	Body  struct {
		Password string `json:"password" doc:"At least 12 characters and at most 256 bytes."`
	}
}

type PermissionsInput struct {
	Login string `path:"login" doc:"The person's login."`
	Body  struct {
		Permissions []string `json:"permissions" doc:"Permission slugs such as monitor:sei:view or monitor:*:check. The actions are view, check, hook and configure."`
	}
}

type DeleteInput struct {
	Login string `path:"login" doc:"The person's login."`
}
