package monitorconfig

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

const precedenceFields = `[
	{"name":"s","label":"S","type":"string","default":"d","env":"S"},
	{"name":"n","label":"N","type":"integer","env":"N"},
	{"name":"p","label":"P","type":"string","secret":true,"env":"P"},
	{"name":"cmd","label":"Cmd","type":"string","editable":false,"reason":"It runs a program.","env":"CMD"},
	{"name":"o","label":"O","type":"object","fields":[
		{"name":"key","label":"Key","type":"string","secret":true},
		{"name":"host","label":"Host","type":"string","default":"localhost"}]},
	{"name":"r","label":"R","type":"list","items":{"type":"object","fields":[{"name":"a","label":"A","type":"string"}]}}
]`

func entryWith(decl *settings.Declaration) catalog.Entry {
	return catalog.Entry{Manifest: catalog.Manifest{Name: "testmonitor"}, Settings: decl}
}

func raws(t *testing.T, doc string) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAssemblePrecedence(t *testing.T) {
	decl := declaration(t, precedenceFields)
	tests := []struct {
		name     string
		document string
		secrets  map[string]string
		ignored  []IgnoredValue
		environ  []string
		want     string
		sources  map[string]Source
		dropped  []plain
		problems []Problem
	}{
		{
			name:     "each origin",
			document: `{"o":{"host":"h"},"r":[{"a":"x"}]}`,
			secrets:  map[string]string{"p": "pw", "o.key": "k1"},
			environ:  []string{"TESTMONITOR__N=7", "TESTMONITOR__CMD=run", "TESTMONITOR__P=other"},
			want:     `{"cmd":"run","n":7,"o":{"host":"h","key":"k1"},"p":"pw","r":[{"a":"x"}],"s":"d"}`,
			sources:  map[string]Source{"s": SourceDefault, "n": SourceEnv, "p": SourcePage, "cmd": SourceEnv, "o": SourcePage, "r": SourcePage},
		},
		{
			name:    "nothing saved and no .env",
			want:    `{"o":{"host":"localhost"},"s":"d"}`,
			sources: map[string]Source{"s": SourceDefault, "n": SourceUnset, "p": SourceUnset, "cmd": SourceUnset, "o": SourceDefault, "r": SourceUnset},
		},
		{
			name:     "empty text saved over a default",
			document: `{"s":""}`,
			environ:  []string{"TESTMONITOR__S=x"},
			want:     `{"o":{"host":"localhost"},"s":"d"}`,
			sources:  map[string]Source{"s": SourcePage, "n": SourceUnset, "p": SourceUnset, "cmd": SourceUnset, "o": SourceDefault, "r": SourceUnset},
		},
		{
			name:     "a value saved in a field that is not editable",
			document: `{"cmd":"x"}`,
			secrets:  map[string]string{"cmd.key": "k-old"},
			environ:  []string{"TESTMONITOR__CMD=run"},
			want:     `{"cmd":"run","o":{"host":"localhost"},"s":"d"}`,
			sources:  map[string]Source{"s": SourceDefault, "n": SourceUnset, "p": SourceUnset, "cmd": SourceEnv, "o": SourceDefault, "r": SourceUnset},
			dropped:  []plain{{"cmd", `"x"`, false, ReasonNotEditable}, {"cmd.key", `"k-old"`, true, ReasonNotEditable}},
		},
		{
			name:     "bad .env text under a saved value",
			document: `{"n":3}`,
			environ:  []string{"TESTMONITOR__N=three"},
			want:     `{"n":3,"o":{"host":"localhost"},"s":"d"}`,
			sources:  map[string]Source{"s": SourceDefault, "n": SourcePage, "p": SourceUnset, "cmd": SourceUnset, "o": SourceDefault, "r": SourceUnset},
		},
		{
			name:     "bad .env text without a saved value",
			environ:  []string{"TESTMONITOR__N=2", "TESTMONITOR__N=three"},
			want:     `{"o":{"host":"localhost"},"s":"d"}`,
			sources:  map[string]Source{"s": SourceDefault, "n": SourceEnv, "p": SourceUnset, "cmd": SourceUnset, "o": SourceDefault, "r": SourceUnset},
			problems: []Problem{{"n", `N is not an integer ("three")`}},
		},
		{
			name:     "a secret saved before the field became secret",
			document: `{"p":"was-plain"}`,
			want:     `{"o":{"host":"localhost"},"p":"was-plain","s":"d"}`,
			sources:  map[string]Source{"s": SourceDefault, "n": SourceUnset, "p": SourcePage, "cmd": SourceUnset, "o": SourceDefault, "r": SourceUnset},
		},
		{
			name:     "saved keys nobody declares",
			document: `{"gone":1,"n":"three"}`,
			secrets:  map[string]string{"s": "hidden", "old": "pw", "o.host": "h"},
			ignored:  []IgnoredValue{{Path: "renamed", Value: json.RawMessage(`"v"`), Reason: ReasonUndeclared}},
			want:     `{"o":{"host":"localhost"},"s":"d"}`,
			sources:  map[string]Source{"s": SourceDefault, "n": SourceUnset, "p": SourceUnset, "cmd": SourceUnset, "o": SourceDefault, "r": SourceUnset},
			dropped: []plain{
				{"n", `"three"`, false, ReasonType},
				{"gone", `1`, false, ReasonUndeclared},
				{"o.host", `"h"`, true, ReasonUndeclared},
				{"old", `"pw"`, true, ReasonUndeclared},
				{"s", `"hidden"`, true, ReasonUndeclared},
				{"renamed", `"v"`, false, ReasonUndeclared},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := Saved{Document: raws(t, cmpOr(tt.document, "{}")), Secrets: tt.secrets, Ignored: tt.ignored}
			a := Assemble(entryWith(decl), tt.environ, "/data", saved)
			if !reflect.DeepEqual(byValue(t, a.Document), byValue(t, []byte(tt.want))) {
				t.Error("the document differs from the one expected")
			}
			if !maps.Equal(a.Sources, tt.sources) {
				t.Errorf("sources = %v, want %v", a.Sources, tt.sources)
			}
			if got := plainIgnored(a.Ignored); !slices.Equal(got, tt.dropped) {
				t.Errorf("ignored = %+v, want %+v", shownIgnored(got), shownIgnored(tt.dropped))
			}
			if !slices.Equal(a.Problems, tt.problems) {
				t.Errorf("problems = %q, want %q", a.Problems, tt.problems)
			}
		})
	}

	t.Run("a document larger than the limit", func(t *testing.T) {
		big := `{"s":"` + strings.Repeat("x", 70<<10) + `"}`
		a := Assemble(entryWith(decl), nil, "/data", Saved{Document: raws(t, big)})
		want := []Problem{{"", "the configuration document is larger than 65536 bytes"}}
		if !slices.Equal(a.Problems, want) {
			t.Errorf("problems = %q, want %q", a.Problems, want)
		}
	})
}

func cmpOr(text, fallback string) string {
	if text == "" {
		return fallback
	}
	return text
}

func TestAssembleEnvironment(t *testing.T) {
	environ := []string{
		"TESTMONITOR__S=x", "TESTMONITOR__P=pw", "TESTMONITOR__OTHER=keep",
		"TESTMONITOR__MONITOR_HUB_CONFIG_FD=9", "PATH=/usr/bin", "TZ=UTC", "HUB_ONLY=1",
	}
	saved := Saved{
		Document: raws(t, `{"s":"y"}`),
		Secrets:  map[string]string{"p": "saved-pw"},
		Variables: map[string]Variable{
			"FREE":  {Value: "v"},
			"PATH":  {Value: "/elsewhere"},
			"S":     {Value: "z"},
			"lower": {Value: "w", Secret: true},
		},
		Ignored: []IgnoredValue{{Path: "renamed", Value: json.RawMessage(`"v"`), Reason: ReasonType}},
	}

	t.Run("with a declaration", func(t *testing.T) {
		decl := declaration(t, `[{"name":"s","label":"S","type":"string","env":"S"},{"name":"p","label":"P","type":"string","secret":true,"env":"P"}]`)
		a := Assemble(entryWith(decl), environ, "/data", saved)
		want := []string{"FREE=v", "MONITOR_HUB_DATA_DIR=/data/monitors/testmonitor", "OTHER=keep", "PATH=/usr/bin", "TMPDIR=/tmp", "TZ=UTC"}
		if !slices.Equal(a.Env, want) {
			t.Errorf("env = %q, want %q", shownEnv(a.Env), want)
		}
		ignored := []plain{
			{"renamed", `"v"`, false, ReasonType},
			{"env:PATH", `"/elsewhere"`, false, ReasonUndeclared},
			{"env:S", `"z"`, false, ReasonUndeclared},
			{"env:lower", `"w"`, true, ReasonUndeclared},
		}
		if got := plainIgnored(a.Ignored); !slices.Equal(got, ignored) {
			t.Errorf("ignored = %+v, want %+v", shownIgnored(got), shownIgnored(ignored))
		}
		if want := map[string]bool{"s": true, "p": true}; !maps.Equal(a.EnvSet, want) {
			t.Errorf("env set = %v, want %v", a.EnvSet, want)
		}
	})

	t.Run("without a declaration", func(t *testing.T) {
		a := Assemble(entryWith(nil), environ, "/data", saved)
		if want := catalog.Environment("testmonitor", environ, "/data", catalog.Layer{}); !slices.Equal(a.Env, want) {
			t.Errorf("env = %q, want %q", shownEnv(a.Env), shownEnv(want))
		}
		if a.Document != nil {
			t.Errorf("document = %s, want none", a.Document)
		}
		ignored := []plain{
			{"s", `"y"`, false, ReasonNoDeclaration},
			{"p", `"saved-pw"`, true, ReasonNoDeclaration},
			{"env:FREE", `"v"`, false, ReasonNoDeclaration},
			{"env:PATH", `"/elsewhere"`, false, ReasonNoDeclaration},
			{"env:S", `"z"`, false, ReasonNoDeclaration},
			{"env:lower", `"w"`, true, ReasonNoDeclaration},
			{"renamed", `"v"`, false, ReasonType},
		}
		if got := plainIgnored(a.Ignored); !slices.Equal(got, ignored) {
			t.Errorf("ignored = %+v, want %+v", shownIgnored(got), shownIgnored(ignored))
		}
	})
}

// shownEnv keeps only the names, since an environment can hold secrets.
func shownEnv(env []string) []string {
	names := make([]string, len(env))
	for i, kv := range env {
		names[i], _, _ = strings.Cut(kv, "=")
	}
	return names
}

func TestAssembleSecrets(t *testing.T) {
	decl := declaration(t, `[
		{"name":"s","label":"S","type":"string","env":"S"},
		{"name":"pw","label":"PW","type":"string","secret":true},
		{"name":"q","label":"Q","type":"string","secret":true,"env":"Q"},
		{"name":"p","label":"P","type":"integer","secret":true,"env":"P"},
		{"name":"o","label":"O","type":"object","fields":[{"name":"key","label":"Key","type":"string","secret":true}]},
		{"name":"mode","label":"Mode","type":"choice","options":["full","summary"],"secret":true}
	]`)
	const (
		savedSecret = "saved-secret-1"
		envSecret   = "env-secret-2"
		freeSecret  = "free-secret-3"
		childSecret = "child-secret-4"
		plainText   = "plain-text-5"
		plainVar    = "plain-var-6"
	)
	environ := []string{"TESTMONITOR__S=" + plainText, "TESTMONITOR__Q=" + envSecret}
	saved := func(pin string) Saved {
		return Saved{
			Secrets:   map[string]string{"pw": savedSecret, "o.key": childSecret, "p": pin},
			Variables: map[string]Variable{"TOKEN": {Value: freeSecret, Secret: true}, "LEVEL": {Value: plainVar}},
		}
	}

	t.Run("the secrets are the secret values", func(t *testing.T) {
		a := Assemble(entryWith(decl), environ, "/data", saved("1234"))
		for _, secret := range []string{savedSecret, envSecret, freeSecret, childSecret, "1234"} {
			if !slices.Contains(a.Secrets, secret) {
				t.Errorf("a secret is missing (%d secrets)", len(a.Secrets))
			}
		}
		for _, plain := range []string{plainText, plainVar} {
			if slices.Contains(a.Secrets, plain) {
				t.Errorf("secrets hold %q", plain)
			}
		}
		if doc := decodeDocument(t, a.Document); doc["p"] != json.Number("1234") {
			t.Error("the saved secret integer is not the number 1234")
		}
	})

	// A value saved before the field became secret may be the old password.
	replaced := func(t *testing.T, document, path, old string) Assembly {
		t.Helper()
		s := saved("1234")
		s.Document = raws(t, document)
		a := Assemble(entryWith(decl), environ, "/data", s)
		want := []plain{{path, old, true, ReasonReplaced}}
		if got := plainIgnored(a.Ignored); !slices.Equal(got, want) {
			t.Errorf("ignored = %+v, want %+v", shownIgnored(got), shownIgnored(want))
		}
		for _, p := range a.Problems {
			if strings.Contains(p.Message, strings.Trim(old, `"`)) {
				t.Error("a problem cites the replaced value")
			}
		}
		return a
	}

	t.Run("a secret saved over a plain value", func(t *testing.T) {
		a := replaced(t, `{"p":4321}`, "p", `4321`)
		if doc := decodeDocument(t, a.Document); doc["p"] != json.Number("1234") {
			t.Error("the document does not hold the saved secret")
		}
	})

	t.Run("a secret child saved over a plain value", func(t *testing.T) {
		a := replaced(t, `{"o":{"key":"old-child-7"}}`, "o.key", `"old-child-7"`)
		doc := decodeDocument(t, a.Document)
		if o, _ := doc["o"].(map[string]any); o["key"] != childSecret {
			t.Error("the document does not hold the saved secret child")
		}
	})

	t.Run("a secret saved as text that does not convert", func(t *testing.T) {
		a := Assemble(entryWith(decl), environ, "/data", saved("12a4"))
		want := []plain{{"p", `"12a4"`, true, ReasonType}}
		if got := plainIgnored(a.Ignored); !slices.Equal(got, want) {
			t.Errorf("ignored = %+v, want %+v", shownIgnored(got), shownIgnored(want))
		}
		if bytes.Contains(a.Document, []byte("12a4")) || len(a.Problems) > 0 {
			t.Error("the text that does not convert reached the document or a problem")
		}
	})

	t.Run("a secret choice saved outside the options", func(t *testing.T) {
		s := saved("1234")
		s.Secrets["mode"] = "FULL"
		a := Assemble(entryWith(decl), environ, "/data", s)
		want := []plain{{"mode", `"FULL"`, true, ReasonType}}
		if got := plainIgnored(a.Ignored); !slices.Equal(got, want) {
			t.Errorf("ignored = %+v, want %+v", shownIgnored(got), shownIgnored(want))
		}
	})

	t.Run("a secret .env text that does not convert", func(t *testing.T) {
		s := saved("")
		delete(s.Secrets, "p")
		a := Assemble(entryWith(decl), append(environ, "TESTMONITOR__P=12a4"), "/data", s)
		if want := []Problem{{"p", "P is not an integer"}}; !slices.Equal(a.Problems, want) {
			t.Errorf("the problems are not %q (%d problems)", want, len(a.Problems))
		}
		for _, p := range a.Problems {
			if strings.Contains(p.Message, "12a4") {
				t.Error("a problem cites the secret text")
			}
		}
	})
}

func TestTheDocumentNeverHoldsNull(t *testing.T) {
	decl := declaration(t, precedenceFields)
	saved := Saved{Document: raws(t, `{"o":{},"r":[{"a":"x"},null],"s":null}`)}
	a := Assemble(entryWith(decl), nil, "/data", saved)
	if bytes.Contains(a.Document, []byte("null")) {
		t.Errorf("document = %s", a.Document)
	}
	want := `{"o":{"host":"localhost"},"r":[{"a":"x"}],"s":"d"}`
	if !reflect.DeepEqual(byValue(t, a.Document), byValue(t, []byte(want))) {
		t.Errorf("document = %s, want %s", a.Document, want)
	}
	ignored := []plain{{"s", `null`, false, ReasonType}, {"r[1]", `null`, false, ReasonType}}
	if got := plainIgnored(a.Ignored); !slices.Equal(got, ignored) {
		t.Errorf("ignored = %+v, want %+v", got, ignored)
	}
}
