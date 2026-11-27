package access

import (
	"slices"
	"testing"
)

func permissions(t *testing.T, slugs ...string) []Permission {
	t.Helper()
	out := make([]Permission, 0, len(slugs))
	for _, slug := range slugs {
		p, err := Parse(slug)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

type ask struct {
	action  Action
	monitor string
	want    bool
}

func checkAsks(t *testing.T, p Principal, asks []ask) {
	t.Helper()
	for _, a := range asks {
		if got := p.Can(a.action, a.monitor); got != a.want {
			t.Errorf("Can(%s, %s) = %v, want %v", a.action, a.monitor, got, a.want)
		}
	}
}

func TestCanUsesTheGrantsOrTheAdminMark(t *testing.T) {
	everything := []ask{}
	for _, monitor := range []string{"sei", "diario"} {
		for _, a := range Actions() {
			everything = append(everything, ask{a, monitor, true})
		}
	}
	tests := map[string]struct {
		person Person
		asks   []ask
	}{
		"an admin without grants": {Person{Admin: true}, everything},
		"view on every monitor": {
			Person{Granted: permissions(t, "monitor:*:view")},
			[]ask{{View, "sei", true}, {View, "diario", true}, {Check, "sei", false}},
		},
		"check on one monitor": {
			Person{Granted: permissions(t, "monitor:sei:check")},
			[]ask{{Check, "sei", true}, {Check, "diario", false}, {View, "sei", false}},
		},
		"configure alone": {
			Person{Granted: permissions(t, "monitor:sei:configure")},
			[]ask{{Configure, "sei", true}, {View, "sei", false}},
		},
		"nothing": {
			Person{},
			[]ask{{View, "sei", false}, {Check, "sei", false}, {Hook, "sei", false}, {Configure, "sei", false}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := NewSession(tt.person)
			if p.Kind() != Session {
				t.Fatalf("Kind() = %v, want Session", p.Kind())
			}
			checkAsks(t, p, tt.asks)
		})
	}
}

func TestNobodyCanDoNothing(t *testing.T) {
	var p Principal
	for _, a := range Actions() {
		if p.Can(a, "sei") {
			t.Errorf("nobody can %s sei", a)
		}
	}
	if p.Admin() {
		t.Error("nobody is an admin")
	}
	if p.Kind() != Nobody {
		t.Errorf("Kind() = %v, want Nobody", p.Kind())
	}
}

func TestATokenNeverExceedsItsOwner(t *testing.T) {
	t.Run("the token carries more than the owner has", func(t *testing.T) {
		owner := Person{ID: 7, Login: "paulo-santos", Granted: permissions(t, "monitor:sei:view")}
		p := NewToken(owner, permissions(t, "monitor:*:view"))
		if p.Kind() != Token || p.UserID() != 7 || p.Login() != "paulo-santos" {
			t.Fatalf("got kind %v, user %d, login %q", p.Kind(), p.UserID(), p.Login())
		}
		checkAsks(t, p, []ask{{View, "sei", true}, {View, "diario", false}})
	})
	t.Run("the owner and the token share nothing", func(t *testing.T) {
		owner := Person{Granted: permissions(t, "monitor:sei:check")}
		checkAsks(t, NewToken(owner, permissions(t, "monitor:sei:view")), []ask{{Check, "sei", false}, {View, "sei", false}})
	})
	t.Run("the token of an admin", func(t *testing.T) {
		p := NewToken(Person{Admin: true}, permissions(t, "monitor:*:view"))
		checkAsks(t, p, []ask{{View, "sei", true}, {View, "diario", true}, {Check, "sei", false}})
		if p.Admin() {
			t.Error("the token of an admin is an admin")
		}
	})
	t.Run("a credential whose owner has nothing", func(t *testing.T) {
		p := NewCredential(Person{}, permissions(t, "monitor:sei:hook"))
		if p.Kind() != Credential {
			t.Fatalf("Kind() = %v, want Credential", p.Kind())
		}
		checkAsks(t, p, []ask{{Hook, "sei", false}})
	})
}

func TestCovers(t *testing.T) {
	tests := map[string]struct {
		principal Principal
		covered   []string
		uncovered []string
	}{
		"an admin": {
			NewSession(Person{Admin: true}),
			[]string{"monitor:*:hook"},
			nil,
		},
		"view on every monitor": {
			NewSession(Person{Granted: permissions(t, "monitor:*:view")}),
			[]string{"monitor:sei:view", "monitor:*:view"},
			nil,
		},
		"view on one monitor": {
			NewSession(Person{Granted: permissions(t, "monitor:sei:view")}),
			[]string{"monitor:sei:view"},
			[]string{"monitor:*:view", "monitor:diario:view"},
		},
		"a token": {
			NewToken(Person{Admin: true, Granted: permissions(t, "monitor:*:view")}, permissions(t, "monitor:*:view")),
			nil,
			[]string{"monitor:sei:view", "monitor:*:view"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, q := range permissions(t, tt.covered...) {
				if !tt.principal.Covers(q) {
					t.Errorf("does not cover %s", q)
				}
			}
			for _, q := range permissions(t, tt.uncovered...) {
				if tt.principal.Covers(q) {
					t.Errorf("covers %s", q)
				}
			}
		})
	}
}

func TestVisibleKeepsTheOrder(t *testing.T) {
	p := NewSession(Person{Granted: permissions(t, "monitor:sei:view", "monitor:anhanguera:view")})
	got := Visible(p, []string{"anhanguera", "diario", "sei"})
	if want := []string{"anhanguera", "sei"}; !slices.Equal(got, want) {
		t.Fatalf("Visible = %v, want %v", got, want)
	}
}
