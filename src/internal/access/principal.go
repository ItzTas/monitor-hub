package access

type Kind int

const (
	Nobody Kind = iota
	Session
	Token
	Credential
)

type Person struct {
	ID      int64
	Login   string
	Admin   bool
	Granted []Permission
}

// Principal is who makes a request. The zero value is nobody and can do nothing.
type Principal struct {
	kind    Kind
	person  Person
	carried []Permission
}

func NewSession(person Person) Principal {
	return Principal{kind: Session, person: person}
}

func NewToken(person Person, carried []Permission) Principal {
	return Principal{kind: Token, person: person, carried: carried}
}

func NewCredential(person Person, carried []Permission) Principal {
	return Principal{kind: Credential, person: person, carried: carried}
}

func (p Principal) Kind() Kind { return p.kind }

func (p Principal) UserID() int64 { return p.person.ID }

func (p Principal) Login() string { return p.person.Login }

// Admin is true only for a session: the token of an admin is not an admin.
func (p Principal) Admin() bool {
	return p.kind == Session && p.person.Admin
}

// Can lets a token or credential do only what both it and its owner can.
func (p Principal) Can(a Action, monitor string) bool {
	if p.kind == Nobody {
		return false
	}
	if !p.person.Admin && !grants(p.person.Granted, a, monitor) {
		return false
	}
	return p.kind == Session || grants(p.carried, a, monitor)
}

// Covers tells whether a session may hand q to a token or credential it creates.
// A monitor:*:<action> is covered only by the same wildcard or the admin mark.
func (p Principal) Covers(q Permission) bool {
	return p.Admin() || p.kind == Session && grants(p.person.Granted, q.Action, q.Monitor)
}

// Visible returns the monitors p can view, in their order.
func Visible(p Principal, monitors []string) []string {
	visible := make([]string, 0, len(monitors))
	for _, name := range monitors {
		if p.Can(View, name) {
			visible = append(visible, name)
		}
	}
	return visible
}
