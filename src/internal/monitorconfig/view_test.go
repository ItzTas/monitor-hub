package monitorconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

const viewFields = `[
	{"name":"watch","label":"Watch","type":"list","items":{"type":"string"},"env":"WATCH"},
	{"name":"interval","label":"Interval","type":"integer","default":30,"env":"INTERVAL"},
	{"name":"db","label":"DB","type":"string","secret":true,"env":"DB"},
	{"name":"token","label":"Token","type":"string","secret":true,"env":"TOKEN"},
	{"name":"cmd","label":"Cmd","type":"string","editable":false,"reason":"It runs a program.","env":"CMD"},
	{"name":"note","label":"Note","type":"string"},
	{"name":"mail","label":"Mail","type":"object","fields":[
		{"name":"host","label":"Host","type":"string","default":"localhost"},
		{"name":"port","label":"Port","type":"integer"},
		{"name":"password","label":"Password","type":"string","secret":true},
		{"name":"key","label":"Key","type":"string","secret":true}]}
]`

var viewEnviron = []string{
	"TESTMONITOR__WATCH=a",
	"TESTMONITOR__INTERVAL=",
	"TESTMONITOR__DB=env-db-secret-value",
	"TESTMONITOR__TOKEN=env-token-secret-value",
	"TESTMONITOR__CMD=x",
	"TESTMONITOR__FREE=f",
	"TESTMONITOR__PROXY=env-proxy-secret-value",
	"TESTMONITOR__EXTRA=",
	"TESTMONITOR__MONITOR_HUB_X=1",
	"TESTMONITOR__LD_PRELOAD=/x.so",
	"TESTMONITOR__PATH=/x",
	"PATH=/bin",
	"HUB_ONLY=1",
}

// viewSaved holds a value from every place a value can be saved.
func viewSaved(t *testing.T) Saved {
	t.Helper()
	return Saved{
		Document: raws(t, `{"watch":["b"],"db":"old-plain-db-value","cmd":"y","mail":{"port":587},"gone":"w"}`),
		Secrets:  map[string]string{"db": "row-db-secret-value", "mail.password": "row-mail-secret-value", "pass": "row-pass-secret-value"},
		Ignored:  []IgnoredValue{{Path: "older", Value: json.RawMessage(`"kept"`), Reason: ReasonUndeclared}},
		Variables: map[string]Variable{
			"FREE":    {Value: "v"},
			"API_KEY": {Value: "row-free-secret-value", Secret: true},
			"PATH":    {Value: "/y"},
			"lower":   {Value: "l"},
		},
		UpdatedAt: savedAt,
	}
}

// viewSecrets are the secret values of the scene, by label.
var viewSecrets = map[string]string{
	"saved secret":            "row-db-secret-value",
	".env secret":             "env-token-secret-value",
	".env secret of a field":  "env-db-secret-value",
	"secret child":            "row-mail-secret-value",
	"secret free variable":    "row-free-secret-value",
	".env-only variable":      "env-proxy-secret-value",
	"replaced ignored value":  "old-plain-db-value",
	"undeclared saved secret": "row-pass-secret-value",
}

type serviceScene struct {
	service *Service
	store   *Postgres
}

func newServiceScene(t *testing.T, entries []catalog.Entry, environ []string, log *slog.Logger) serviceScene {
	t.Helper()
	store := newStore(t)
	return serviceScene{service: NewService(Deps{Entries: entries, Environ: environ, DataRoot: "/data", Store: store, Log: log}), store: store}
}

func (s serviceScene) put(t *testing.T, monitor string, saved Saved) {
	t.Helper()
	if _, err := s.store.Put(t.Context(), monitor, saved, 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// masked prints v with every secret of the tests in this file hidden.
func masked(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := string(data)
	for _, secret := range viewSecrets {
		out = strings.ReplaceAll(out, secret, "redacted")
	}
	for _, secret := range []string{"renamed-secret-1", "plain-then-secret-2", "no-decl-secret-3", "no-decl-secret-4"} {
		out = strings.ReplaceAll(out, secret, "redacted")
	}
	return out
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func readView(t *testing.T, s *Service, name string) View {
	t.Helper()
	view, err := s.Read(t.Context(), name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return view
}

func text(s string) *string { return &s }

func TestReadShowsWhereEachValueComesFrom(t *testing.T) {
	decl := declaration(t, viewFields)
	scene := newServiceScene(t, []catalog.Entry{entryWith(decl)}, viewEnviron, discard())
	scene.put(t, "testmonitor", viewSaved(t))

	want := View{
		Monitor:     "testmonitor",
		Declared:    true,
		Declaration: decl,
		Revision:    1,
		Document: map[string]any{
			"watch":    []any{"b"},
			"interval": json.Number("30"),
			"mail":     map[string]any{"host": "localhost", "port": json.Number("587")},
		},
		Fields: []FieldState{
			{Name: "watch", Source: SourcePage, Saved: true, EnvSet: true},
			{Name: "interval", Source: SourceDefault},
			{Name: "db", Source: SourcePage, Saved: true, EnvSet: true},
			{Name: "token", Source: SourceEnv, EnvSet: true},
			{Name: "cmd", Source: SourceEnv, EnvSet: true},
			{Name: "note", Source: SourceUnset},
			{Name: "mail", Source: SourcePage, Saved: true},
		},
		Secrets: []SecretState{
			{Path: "db", Set: true, Saved: true, EnvSet: true},
			{Path: "token", Set: true, EnvSet: true},
			{Path: "mail.password", Set: true, Saved: true},
			{Path: "mail.key"},
		},
		Ignored: []IgnoredView{
			{Path: "db", Secret: true, Reason: ReasonReplaced},
			{Path: "cmd", Value: json.RawMessage(`"y"`), Reason: ReasonNotEditable},
			{Path: "gone", Value: json.RawMessage(`"w"`), Reason: ReasonUndeclared},
			{Path: "pass", Secret: true, Reason: ReasonUndeclared},
			{Path: "older", Value: json.RawMessage(`"kept"`), Reason: ReasonUndeclared},
			{Path: "env:PATH", Value: json.RawMessage(`"/y"`), Reason: ReasonUndeclared},
			{Path: "env:lower", Value: json.RawMessage(`"l"`), Reason: ReasonUndeclared},
		},
		Variables: []VariableView{
			{Name: "API_KEY", Secret: true, Source: SourcePage, Saved: true},
			{Name: "EXTRA", Secret: true, Source: SourceEnv},
			{Name: "FREE", Source: SourcePage, Value: text("v"), Saved: true, EnvSet: true},
			{Name: "PROXY", Secret: true, Source: SourceEnv, EnvSet: true},
		},
	}
	if got := readView(t, scene.service, "testmonitor"); !reflect.DeepEqual(got, want) {
		t.Errorf("Read =\n%s\nwant\n%s", masked(t, got), masked(t, want))
	}
}

func TestReadNeverReturnsASecret(t *testing.T) {
	scene := newServiceScene(t, []catalog.Entry{entryWith(declaration(t, viewFields))}, viewEnviron, discard())
	scene.put(t, "testmonitor", viewSaved(t))

	data, err := json.Marshal(readView(t, scene.service, "testmonitor"))
	if err != nil {
		t.Fatalf("encode the view: %v", err)
	}
	for label, secret := range viewSecrets {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("the view shows the %s", label)
		}
	}
}

func TestReadShowsIgnoredValues(t *testing.T) {
	fields := `[{"name":"host","label":"Host","type":"string"},{"name":"password","label":"Password","type":"string","secret":true}]`
	scene := newServiceScene(t, []catalog.Entry{entryWith(declaration(t, fields))}, nil, discard())
	scene.put(t, "testmonitor", Saved{
		Document: raws(t, `{"host_name":"mail.example.test"}`),
		Secrets:  map[string]string{"pass": "renamed-secret-1"},
		// Stored before password became secret.
		Ignored:   []IgnoredValue{{Path: "password", Value: json.RawMessage(`"plain-then-secret-2"`), Reason: ReasonType}},
		UpdatedAt: savedAt,
	})

	want := []IgnoredView{
		{Path: "host_name", Value: json.RawMessage(`"mail.example.test"`), Reason: ReasonUndeclared},
		{Path: "pass", Secret: true, Reason: ReasonUndeclared},
		{Path: "password", Reason: ReasonType},
	}
	if got := readView(t, scene.service, "testmonitor").Ignored; !reflect.DeepEqual(got, want) {
		t.Errorf("Ignored = %s, want %s", masked(t, got), masked(t, want))
	}
}

func TestReadWithoutDeclaration(t *testing.T) {
	environ := []string{"TESTMONITOR__N=3", "TESTMONITOR__PROXY=p"}
	scene := newServiceScene(t, []catalog.Entry{entryWith(nil)}, environ, discard())
	scene.put(t, "testmonitor", Saved{
		Document:  raws(t, `{"n":7}`),
		Secrets:   map[string]string{"p": "no-decl-secret-3"},
		Variables: map[string]Variable{"FREE": {Value: "v"}, "KEY": {Value: "no-decl-secret-4", Secret: true}},
		UpdatedAt: savedAt,
	})

	want := View{
		Monitor:  "testmonitor",
		Revision: 1,
		Ignored: []IgnoredView{
			{Path: "n", Value: json.RawMessage(`7`), Reason: ReasonNoDeclaration},
			{Path: "p", Secret: true, Reason: ReasonNoDeclaration},
			{Path: "env:FREE", Value: json.RawMessage(`"v"`), Reason: ReasonNoDeclaration},
			{Path: "env:KEY", Secret: true, Reason: ReasonNoDeclaration},
		},
	}
	if got := readView(t, scene.service, "testmonitor"); !reflect.DeepEqual(got, want) {
		t.Errorf("Read = %s, want %s", masked(t, got), masked(t, want))
	}
}

func TestReadShowsEnvProblems(t *testing.T) {
	scene := newServiceScene(t, []catalog.Entry{entryWith(declaration(t, integerFields))}, []string{"TESTMONITOR__N=three"}, discard())

	view := readView(t, scene.service, "testmonitor")
	if want := []Problem{{"n", `N is not an integer ("three")`}}; !reflect.DeepEqual(view.Problems, want) {
		t.Errorf("Problems = %v, want %v", view.Problems, want)
	}
	if want := (FieldState{Name: "n", Source: SourceEnv, EnvSet: true}); view.Fields[0] != want {
		t.Errorf("field n = %+v, want %+v", view.Fields[0], want)
	}
	if view.Revision != 0 {
		t.Errorf("Revision = %d without a saved row, want 0", view.Revision)
	}
}

func TestReadOfAnUnknownMonitorAndADownDatabase(t *testing.T) {
	store := &flakyStore{down: true}
	s := NewService(Deps{Entries: []catalog.Entry{entryWith(declaration(t, integerFields))}, Store: store, Log: discard()})

	if _, err := s.Read(t.Context(), "nope"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Read of an unknown monitor = %v, want ErrUnknown", err)
	}
	if store.gets != 0 {
		t.Errorf("Read of an unknown monitor read the database %d times", store.gets)
	}
	if _, err := s.Read(t.Context(), "testmonitor"); !errors.Is(err, ErrDatabase) {
		t.Errorf("Read with the database down = %v, want ErrDatabase", err)
	}
}

func TestRedact(t *testing.T) {
	cases := []struct {
		name, text string
		secrets    []string
		want       string
	}{
		{"every occurrence", "a hunter2x b hunter2x", []string{"hunter2x", "abc"}, "a [redacted] b [redacted]"},
		{"a secret inside a longer one", "pw=hunter2x9 and hunter2", []string{"hunter2", "hunter2x9"}, "pw=[redacted] and [redacted]"},
		{"secrets that overlap", "xabcdefghx", []string{"abcdef", "defghi", "efgh"}, "x[redacted]x"},
		{"a secret that repeats itself", "aaaaa", []string{"aaaa"}, "[redacted]"},
		{"shorter than 4 bytes", "abc ab", []string{"abc", "ab", ""}, "abc ab"},
		{"none", "nothing here", nil, "nothing here"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redact(c.text, c.secrets); got != c.want {
				t.Errorf("redact = %q, want %q", got, c.want)
			}
		})
	}
}

// failingFor fails every read of one monitor.
type failingFor struct {
	Store
	monitor string
}

func (s failingFor) Get(ctx context.Context, monitor string) (Saved, error) {
	if monitor == s.monitor {
		return Saved{}, errors.New("dial tcp 127.0.0.1:5432: connection refused")
	}
	return s.Store.Get(ctx, monitor)
}

func TestWarnIgnoredNamesOnlyPaths(t *testing.T) {
	named := func(name string, decl *settings.Declaration) catalog.Entry {
		return catalog.Entry{Manifest: catalog.Manifest{Name: name}, Settings: decl}
	}
	entries := []catalog.Entry{
		entryWith(declaration(t, viewFields)),
		named("clean", declaration(t, integerFields)),
		named("down", declaration(t, integerFields)),
		named("bare", nil),
	}
	store := newStore(t)
	var buf bytes.Buffer
	s := NewService(Deps{Entries: entries, Environ: viewEnviron, DataRoot: "/data", Store: failingFor{store, "down"}, Log: slog.New(slog.NewJSONHandler(&buf, nil))})
	for name, saved := range map[string]Saved{
		"testmonitor": viewSaved(t),
		"clean":       {Document: raws(t, `{"n":4}`), UpdatedAt: savedAt},
		"bare":        {Document: raws(t, `{"n":"bare-plain-5"}`), UpdatedAt: savedAt},
	} {
		if _, err := store.Put(t.Context(), name, saved, 0); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}

	s.WarnIgnored(t.Context())

	want := []map[string]any{
		{"level": "WARN", "msg": "saved configuration values are not applied", "monitor": "testmonitor", "paths": []any{"db", "cmd", "gone", "pass", "older", "env:PATH", "env:lower"}},
		{"level": "WARN", "msg": "the saved configuration could not be read", "monitor": "down", "error": "dial tcp 127.0.0.1:5432: connection refused"},
		{"level": "WARN", "msg": "saved configuration values are not applied", "monitor": "bare", "paths": []any{"n"}},
	}
	// Records carry only paths, so they can be printed.
	if got := logRecords(t, &buf); !reflect.DeepEqual(got, want) {
		t.Errorf("log records = %v, want %v", got, want)
	}
	values := map[string]string{"plain ignored value": `"w"`, "not editable value": `"y"`, "kept value": "kept", "plain value without declaration": "bare-plain-5"}
	for label, secret := range viewSecrets {
		values[label] = secret
	}
	for label, value := range values {
		if strings.Contains(buf.String(), value) {
			t.Errorf("the log shows the %s", label)
		}
	}
}
