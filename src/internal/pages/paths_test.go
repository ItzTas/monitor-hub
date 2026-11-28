package pages

import (
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

func TestControlIDs(t *testing.T) {
	ids := map[string]string{
		"sei_url":              "f-sei_url",
		"rules[0].process":     "f-rules-0--process",
		"rules[12].actions[3]": "f-rules-12--actions-3",
		"account.token":        "f-account--token",
	}

	for path, want := range ids {
		if got := controlID(path); got != want {
			t.Errorf("controlID(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDeclarationKey(t *testing.T) {
	view := func(change func(*monitorconfig.View)) monitorconfig.View {
		v := monitorconfig.View{Declared: true, Declaration: &settings.Declaration{Fields: []settings.Field{
			{Name: "level", Label: "Level", Type: settings.Choice, Options: []string{"low", "high"}, Editable: true},
		}}}
		change(&v)
		return v
	}
	same := func(*monitorconfig.View) {}
	base := declarationKey(view(same))

	if again := declarationKey(view(same)); again != base {
		t.Errorf("the same View gave the keys %q and %q", base, again)
	}
	changes := map[string]func(*monitorconfig.View){
		"another label":  func(v *monitorconfig.View) { v.Declaration.Fields[0].Label = "Detail level" },
		"another option": func(v *monitorconfig.View) { v.Declaration.Fields[0].Options = []string{"low", "loud"} },
		"one more field": func(v *monitorconfig.View) {
			v.Declaration.Fields = append(v.Declaration.Fields, settings.Field{Name: "count", Label: "Count", Type: settings.Integer, Editable: true})
		},
		"not declared": func(v *monitorconfig.View) { v.Declared = false },
	}
	for name, change := range changes {
		if got := declarationKey(view(change)); got == base {
			t.Errorf("a View with %s has the key of the first one", name)
		}
	}
}
