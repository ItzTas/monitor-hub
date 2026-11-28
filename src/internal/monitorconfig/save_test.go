package monitorconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

const saveFields = `[
	{"name":"watch","label":"Watch","type":"list","items":{"type":"string"},"env":"WATCH"},
	{"name":"n","label":"N","type":"integer","env":"N"},
	{"name":"db","label":"DB","type":"string","secret":true,"env":"DB"},
	{"name":"pin","label":"PIN","type":"integer","secret":true},
	{"name":"level","label":"Level","type":"choice","options":["low","high"],"secret":true},
	{"name":"mode","label":"Mode","type":"choice","options":["full","summary"]},
	{"name":"cmd","label":"Cmd","type":"string","editable":false,"reason":"It runs a program.","env":"CMD"},
	{"name":"signing","label":"Signing","type":"string","secret":true,"editable":false,"reason":"It signs requests.","env":"SIGNING"},
	{"name":"mail","label":"Mail","type":"object","fields":[
		{"name":"host","label":"Host","type":"string","default":"localhost"},
		{"name":"port","label":"Port","type":"integer"},
		{"name":"password","label":"Password","type":"string","secret":true}]},
	{"name":"rules","label":"Rules","type":"list","items":{"type":"object","fields":[{"name":"a","label":"A","type":"string"}]}},
	{"name":"proxy","label":"Proxy","type":"object","fields":[
		{"name":"auth","label":"Auth","type":"object","fields":[{"name":"token","label":"Token","type":"string","secret":true}]}]}
]`

var saveEnviron = []string{
	"TESTMONITOR__WATCH=a",
	"TESTMONITOR__N=5",
	"TESTMONITOR__CMD=x",
	"PATH=/bin",
}

var startedAt = time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)

type fakeSupervisor struct {
	mu       sync.Mutex
	restarts int
	attempt  supervisor.Attempt
	client   monitorpb.MonitorClient
}

func (s *fakeSupervisor) Restart(string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restarts++
	return uint64(s.restarts), nil
}

func (s *fakeSupervisor) LastAttempt(string) supervisor.Attempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempt
}

func (s *fakeSupervisor) Client(string) (monitorpb.MonitorClient, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client, s.client != nil
}

func (s *fakeSupervisor) setAttempt(a supervisor.Attempt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempt = a
}

func (s *fakeSupervisor) setClient(c monitorpb.MonitorClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = c
}

// settle ends the last restart as if the monitor started and later exited.
func (s *fakeSupervisor) settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := uint64(s.restarts)
	s.attempt = supervisor.Attempt{Request: n, Phase: supervisor.PhaseExited, Settled: n}
}

func (s *fakeSupervisor) restartCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// fakeChecker keeps what it received and answers what the test sets. With entered set, it
// signals each call there and waits for release.
type fakeChecker struct {
	mu       sync.Mutex
	calls    int
	env      []string
	document []byte
	problems []Problem
	err      error
	entered  chan struct{}
	release  chan struct{}
}

func (c *fakeChecker) Validate(_ context.Context, _ catalog.Entry, env []string, document []byte) ([]Problem, error) {
	c.mu.Lock()
	c.calls++
	c.env, c.document = env, document
	problems, err, entered, release := c.problems, c.err, c.entered, c.release
	c.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
		<-release
	}
	return problems, err
}

func (c *fakeChecker) answer(problems []Problem, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.problems, c.err = problems, err
}

func (c *fakeChecker) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

type saveScene struct {
	service    *Service
	store      *Postgres
	supervisor *fakeSupervisor
	checker    *fakeChecker
	clock      *fakeClock
	log        *bytes.Buffer
	entry      catalog.Entry
	environ    []string
}

// newSaveScene serves testmonitor, with the declaration fields, and bare, without one.
func newSaveScene(t *testing.T, fields string, environ []string) *saveScene {
	t.Helper()
	return newSaveSceneOn(t, newStore(t), fields, environ)
}

func newSaveSceneOn(t *testing.T, store Store, fields string, environ []string) *saveScene {
	t.Helper()
	entry := entryWith(declaration(t, fields))
	bare := catalog.Entry{Manifest: catalog.Manifest{Name: "bare"}}
	s := &saveScene{
		supervisor: &fakeSupervisor{},
		checker:    &fakeChecker{},
		clock:      &fakeClock{now: startedAt},
		log:        &bytes.Buffer{},
		entry:      entry,
		environ:    environ,
	}
	if p, ok := store.(*Postgres); ok {
		s.store = p
	}
	s.service = NewService(Deps{
		Entries:    []catalog.Entry{entry, bare},
		Environ:    environ,
		DataRoot:   "/data",
		Store:      store,
		Supervisor: s.supervisor,
		Checker:    s.checker,
		Log:        slog.New(slog.NewJSONHandler(s.log, nil)),
		Now:        s.clock.Now,
	})
	return s
}

func (s *saveScene) put(t *testing.T, monitor string, saved Saved) {
	t.Helper()
	if _, err := s.store.Put(t.Context(), monitor, saved, 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func (s *saveScene) stored(t *testing.T, monitor string) Saved {
	t.Helper()
	saved, err := s.store.Get(t.Context(), monitor)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return saved
}

func (s *saveScene) save(t *testing.T, change Change) SaveResult {
	t.Helper()
	result, err := s.service.Save(t.Context(), "testmonitor", change)
	if err != nil {
		t.Fatalf("Save: %s", maskedError(err))
	}
	if result.Restarted {
		s.supervisor.settle()
	}
	return result
}

// maskedError prints an error of these tests with their secrets hidden.
func maskedError(err error) string {
	out := err.Error()
	var problems *ProblemsError
	if errors.As(err, &problems) {
		data, _ := json.Marshal(problems.Problems)
		out += " " + string(data)
	}
	for _, secret := range saveSecrets {
		out = strings.ReplaceAll(out, secret, "redacted")
	}
	return out
}

// saveSecrets are the secret values of the save tests, by label.
var saveSecrets = map[string]string{
	"new secret":            "new-db-secret-value",
	"stored secret":         "row-db-secret-value",
	"old secret":            "old-db-secret-value",
	"stored child secret":   "first-mail-secret",
	"replaced secret":       "first-db-secret",
	"replacing secret":      "second-db-secret",
	"stored removed secret": "old-secret-value",
	"replaced child value":  "old-mail-plain",
	"secret variable":       "free-secret-value",
	"page secret":           "pw-secret-value",
	"plain value of a field that became secret": "plain-old-value",
}

func revision(r int64) *int64 { return &r }

func problemsOf(t *testing.T, err error) []Problem {
	t.Helper()
	var problems *ProblemsError
	if !errors.As(err, &problems) {
		t.Fatalf("Save = %v, want a *ProblemsError", err)
	}
	if err.Error() != "the values have problems" {
		t.Errorf("Error() = %q", err.Error())
	}
	return problems.Problems
}

// sameSaved compares the stored parts by value without printing them: they hold secrets.
func sameSaved(t *testing.T, what string, got, want Saved) {
	t.Helper()
	parts := func(saved Saved) Saved {
		return Saved{
			Document:  orEmpty(saved.Document),
			Secrets:   orEmpty(saved.Secrets),
			Ignored:   slices.Clip(saved.Ignored),
			Variables: orEmpty(saved.Variables),
		}
	}
	if !reflect.DeepEqual(asJSON(t, parts(got)), asJSON(t, parts(want))) {
		t.Errorf("%s: the stored configuration differs (values not shown)", what)
	}
}

func TestSaveValidatesWritesAndRestarts(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)

	result := s.save(t, Change{
		Document: raws(t, `{"n":7,"mail":{"port":587}}`),
		Secrets:  map[string]*string{"db": text("new-db-secret-value")},
		Variables: VariableChange{
			Set:    map[string]string{"FREE": "v", "API_KEY": "free-secret-value"},
			Secret: []string{"API_KEY"},
		},
	})

	want := SaveResult{Changed: []string{"mail", "n", "db", "env:API_KEY", "env:FREE"}, Restarted: true, Request: 1}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("Save = %+v, want %+v", result, want)
	}
	stored := s.stored(t, "testmonitor")
	sameSaved(t, "after the save", stored, Saved{
		Document:  raws(t, `{"n":7,"mail":{"port":587}}`),
		Secrets:   map[string]string{"db": "new-db-secret-value"},
		Variables: map[string]Variable{"FREE": {Value: "v"}, "API_KEY": {Value: "free-secret-value", Secret: true}},
	})
	if stored.Revision != 1 || !stored.UpdatedAt.Equal(startedAt) {
		t.Errorf("revision %d at %v, want 1 at %v", stored.Revision, stored.UpdatedAt, startedAt)
	}
	after := Assemble(s.entry, s.environ, "/data", stored)
	if !slices.Equal(s.checker.env, after.Env) || !bytes.Equal(s.checker.document, after.Document) {
		t.Error("the checker did not receive the environment and the document of the saved configuration")
	}
	if n := s.supervisor.restartCount(); n != 1 {
		t.Errorf("%d restarts, want 1", n)
	}
	records := []map[string]any{{
		"level": "INFO", "msg": "monitor configuration saved", "monitor": "testmonitor",
		"changed": []any{"mail", "n", "db", "env:API_KEY", "env:FREE"}, "restarted": true,
	}}
	if got := logRecords(t, s.log); !reflect.DeepEqual(got, records) {
		t.Errorf("log records = %s, want %v", maskedError(errors.New(s.log.String())), records)
	}
}

func TestSaveRefusesHubProblemsWithoutWriting(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	initial := Saved{
		Document: raws(t, `{"n":7}`),
		Secrets:  map[string]string{"db": "row-db-secret-value"},
		Ignored:  []IgnoredValue{{Path: "older", Value: json.RawMessage(`"kept"`), Reason: ReasonUndeclared}},
	}
	s.put(t, "testmonitor", initial)
	s.put(t, "bare", Saved{Document: raws(t, `{"n":7}`)})
	long := strings.Repeat("x", 8193)
	declared := "settings.json does not declare this field"
	secret := "this field is secret: send it in secrets"
	notEditable := "the monitor does not allow changing this field here"
	nul := "the value holds a NUL character"
	tooLong := "the value is longer than 8192 bytes"
	noSecret := "settings.json declares no secret field at this path"
	badName := "the name must match ^[A-Z][A-Z0-9_]*$"
	marks := "secret only marks undeclared variables that this request sets"

	cases := []struct {
		name    string
		monitor string
		change  Change
		want    []Problem
	}{
		{"a monitor without a declaration", "bare", Change{Document: raws(t, `{"n":1}`)},
			[]Problem{{"", "the monitor ships no readable settings.json version 2, so its configuration cannot be changed here"}}},
		{"a variable for a monitor without a declaration", "bare", Change{Variables: VariableChange{Unset: []string{"FREE"}}},
			[]Problem{{"", "the monitor ships no readable settings.json version 2, so its configuration cannot be changed here"}}},
		{"an undeclared field", "", Change{Document: raws(t, `{"nope":1}`)}, []Problem{{"nope", declared}}},
		{"a secret field in document", "", Change{Document: raws(t, `{"db":"x"}`)}, []Problem{{"db", secret}}},
		{"null on a secret field in document", "", Change{Document: raws(t, `{"db":null}`)}, []Problem{{"db", secret}}},
		{"a secret child in document", "", Change{Document: raws(t, `{"mail":{"password":"x","port":25}}`)}, []Problem{{"mail.password", secret}}},
		{"a secret deeper in objects", "", Change{Document: raws(t, `{"proxy":{"auth":{"token":"x"}}}`)}, []Problem{{"proxy.auth.token", secret}}},
		{"a field that is not editable", "", Change{Document: raws(t, `{"cmd":"y"}`)}, []Problem{{"cmd", notEditable}}},
		{"a secret that is not editable", "", Change{Secrets: map[string]*string{"signing": text("s")}}, []Problem{{"signing", notEditable}}},
		{"a value of the wrong type", "", Change{Document: raws(t, `{"n":"three"}`)}, []Problem{{"n", "use a whole number"}}},
		{"null inside an object", "", Change{Document: raws(t, `{"mail":{"port":null}}`)}, []Problem{{"mail.port", "use a whole number"}}},
		{"parts of an object", "", Change{Document: raws(t, `{"mail":{"port":"x","extra":1,"host":"h"}}`)},
			[]Problem{{"mail.extra", declared}, {"mail.port", "use a whole number"}}},
		{"a list item", "", Change{Document: raws(t, `{"watch":["a",3]}`)}, []Problem{{"watch[1]", "use text"}}},
		{"a key in a list item", "", Change{Document: raws(t, `{"rules":[{"a":"x","b":1}]}`)}, []Problem{{"rules[0].b", declared}}},
		{"an object list item", "", Change{Document: raws(t, `{"rules":[3]}`)}, []Problem{{"rules[0]", "use an object"}}},
		{"a choice outside the options", "", Change{Document: raws(t, `{"mode":"FULL"}`)}, []Problem{{"mode", "use one of: full, summary"}}},
		{"text with NUL", "", Change{Document: raws(t, `{"watch":["a\u0000b"]}`)}, []Problem{{"watch[0]", nul}}},
		{"text inside a refused value", "", Change{Document: raws(t, `{"n":["a\u0000b"],"mail":{"extra":{"x":"a\u0000b"}}}`)},
			[]Problem{{"mail.extra", declared}, {"n", "use a whole number"}}},
		{"long text", "", Change{Document: raws(t, `{"mail":{"host":"`+long+`"}}`)}, []Problem{{"mail.host", tooLong}}},
		{"a secret with NUL", "", Change{Secrets: map[string]*string{"db": text("a\x00b")}}, []Problem{{"db", nul}}},
		{"a long secret", "", Change{Secrets: map[string]*string{"db": text(long)}}, []Problem{{"db", tooLong}}},
		{"a secret at a plain field", "", Change{Secrets: map[string]*string{"n": text("3")}}, []Problem{{"n", noSecret}}},
		{"a secret at a plain child", "", Change{Secrets: map[string]*string{"mail.host": text("h")}}, []Problem{{"mail.host", noSecret}}},
		{"a secret at an undeclared path", "", Change{Secrets: map[string]*string{"nope": nil}}, []Problem{{"nope", noSecret}}},
		{"secret text that does not convert", "", Change{Secrets: map[string]*string{"pin": text("12a4")}}, []Problem{{"pin", "use a whole number"}}},
		{"a secret choice outside the options", "", Change{Secrets: map[string]*string{"level": text("LOW")}}, []Problem{{"level", "use one of: low, high"}}},
		{"an ignored path that is not there", "", Change{DeleteIgnored: []string{"nope"}}, []Problem{{"ignored:nope", "no ignored value has this path"}}},
		{"set and unset", "", Change{Variables: VariableChange{Set: map[string]string{"FREE": "1"}, Unset: []string{"FREE"}}},
			[]Problem{{"env:FREE", "set and unset name the same variable"}}},
		{"an invalid name to set", "", Change{Variables: VariableChange{Set: map[string]string{"lower": "1"}}}, []Problem{{"env:lower", badName}}},
		{"an invalid name to unset", "", Change{Variables: VariableChange{Unset: []string{"9X"}}}, []Problem{{"env:9X", badName}}},
		{"a reserved name", "", Change{Variables: VariableChange{Set: map[string]string{"PATH": "/x", "LD_PRELOAD": "x"}}},
			[]Problem{{"env:LD_PRELOAD", "the hub reserves this name"}, {"env:PATH", "the hub reserves this name"}}},
		{"the env of a field", "", Change{Variables: VariableChange{Set: map[string]string{"DB": "x"}}},
			[]Problem{{"env:DB", "this name is a field of the monitor; change the field instead"}}},
		{"a variable with NUL", "", Change{Variables: VariableChange{Set: map[string]string{"FREE": "a\x00b"}}}, []Problem{{"env:FREE", nul}}},
		{"a long variable", "", Change{Variables: VariableChange{Set: map[string]string{"FREE": long}}}, []Problem{{"env:FREE", tooLong}}},
		{"secret without set", "", Change{Variables: VariableChange{Set: map[string]string{"FREE": "1"}, Secret: []string{"OTHER"}}},
			[]Problem{{"env:OTHER", marks}}},
		{"secret on the env of a field", "", Change{Variables: VariableChange{Secret: []string{"N"}}}, []Problem{{"env:N", marks}}},
		{"every problem at once, by path", "", Change{
			Document:      raws(t, `{"nope":1,"cmd":"y"}`),
			Secrets:       map[string]*string{"n": text("3")},
			Variables:     VariableChange{Set: map[string]string{"lower": "1"}},
			DeleteIgnored: []string{"x"},
		}, []Problem{{"cmd", notEditable}, {"env:lower", badName}, {"ignored:x", "no ignored value has this path"}, {"n", noSecret}, {"nope", declared}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			monitor := cmpOr(c.monitor, "testmonitor")
			_, err := s.service.Save(t.Context(), monitor, c.change)
			if got := problemsOf(t, err); !reflect.DeepEqual(got, c.want) {
				t.Errorf("problems = %s, want %v", maskedError(err), c.want)
			}
		})
	}
	if n := s.checker.callCount(); n != 0 {
		t.Errorf("the checker ran %d times", n)
	}
	if n := s.supervisor.restartCount(); n != 0 {
		t.Errorf("%d restarts", n)
	}
	if got := s.stored(t, "testmonitor"); got.Revision != 1 {
		t.Errorf("revision %d, want 1", got.Revision)
	}
	sameSaved(t, "testmonitor", s.stored(t, "testmonitor"), initial)

	t.Run("text of exactly 8192 bytes", func(t *testing.T) {
		fits := strings.Repeat("x", 8192)
		s.save(t, Change{
			Document:  raws(t, `{"mail":{"host":"`+fits+`"}}`),
			Secrets:   map[string]*string{"db": &fits},
			Variables: VariableChange{Set: map[string]string{"FREE": fits}},
		})
	})
}

func TestSaveRefusesWhatTheMonitorRefuses(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	initial := Saved{Secrets: map[string]string{"db": "row-db-secret-value"}}
	s.put(t, "testmonitor", initial)

	s.checker.answer([]Problem{{"n", "set at least one name"}}, nil)
	_, err := s.service.Save(t.Context(), "testmonitor", Change{Document: raws(t, `{"n":7}`)})
	if got, want := problemsOf(t, err), []Problem{{"n", "set at least one name"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("problems = %v, want %v", got, want)
	}

	s.checker.answer(nil, &ValidatorError{Reason: "it exited with code 2", Stderr: "boom row-db-secret-value"})
	_, err = s.service.Save(t.Context(), "testmonitor", Change{Document: raws(t, `{"n":7}`)})
	var validatorErr *ValidatorError
	if !errors.As(err, &validatorErr) || validatorErr.Reason != "it exited with code 2" {
		t.Errorf("Save = %s, want the validator's error", maskedError(err))
	}

	if n := s.supervisor.restartCount(); n != 0 {
		t.Errorf("%d restarts", n)
	}
	sameSaved(t, "after the refusals", s.stored(t, "testmonitor"), initial)
	records := []map[string]any{{
		"level": "WARN", "msg": "the monitor could not validate its configuration", "monitor": "testmonitor",
		"reason": "it exited with code 2", "stderr": "boom [redacted]",
	}}
	if got := logRecords(t, s.log); !reflect.DeepEqual(got, records) {
		t.Errorf("log records = %s, want %v", maskedError(errors.New(s.log.String())), records)
	}
}

func TestSaveRedactsSecretsInWhatTheMonitorSays(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.put(t, "testmonitor", Saved{Secrets: map[string]string{"db": "old-db-secret-value"}})
	change := Change{Secrets: map[string]*string{"db": text("new-db-secret-value")}}

	s.checker.answer([]Problem{{"db", "cannot parse new-db-secret-value"}}, nil)
	_, err := s.service.Save(t.Context(), "testmonitor", change)
	if got, want := problemsOf(t, err), []Problem{{"db", "cannot parse [redacted]"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("problems = %s, want %v", maskedError(err), want)
	}

	s.checker.answer(nil, &ValidatorError{
		Reason: `problem 1 names the path "new-db-secret-value", which settings.json does not declare`,
		Stderr: "it was old-db-secret-value",
	})
	_, err = s.service.Save(t.Context(), "testmonitor", change)
	var validatorErr *ValidatorError
	if !errors.As(err, &validatorErr) {
		t.Fatalf("Save = %s, want a *ValidatorError", maskedError(err))
	}
	reason := `problem 1 names the path "[redacted]", which settings.json does not declare`
	if validatorErr.Reason != reason || validatorErr.Stderr != "it was [redacted]" {
		t.Errorf("the returned error holds a secret: %s", maskedError(err))
	}
	records := []map[string]any{{
		"level": "WARN", "msg": "the monitor could not validate its configuration", "monitor": "testmonitor",
		"reason": reason, "stderr": "it was [redacted]",
	}}
	if got := logRecords(t, s.log); !reflect.DeepEqual(got, records) {
		t.Errorf("log records = %s, want %v", maskedError(errors.New(s.log.String())), records)
	}
}

func TestASaveThatLeavesTheDocumentAloneDoesNotRestart(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.put(t, "testmonitor", Saved{
		Document: raws(t, `{"n":5}`),
		Ignored:  []IgnoredValue{{Path: "older", Value: json.RawMessage(`"kept"`), Reason: ReasonUndeclared}},
	})

	if got := s.save(t, Change{Document: raws(t, `{"n":5}`)}); !reflect.DeepEqual(got, SaveResult{}) {
		t.Errorf("saving the stored value = %+v, want nothing", got)
	}
	if got := s.stored(t, "testmonitor").Revision; got != 1 {
		t.Errorf("saving the stored value wrote revision %d", got)
	}

	steps := []struct {
		name   string
		change Change
		want   []string
	}{
		{"null where the .env has the same value", Change{Document: map[string]json.RawMessage{"n": json.RawMessage(`null`)}}, []string{"n"}},
		{"the value the .env has", Change{Document: raws(t, `{"n":5}`)}, []string{"n"}},
		{"a null that decoded to nothing", Change{Document: map[string]json.RawMessage{"n": nil}}, []string{"n"}},
		{"deleting an ignored value", Change{DeleteIgnored: []string{"older"}}, []string{"ignored:older"}},
	}
	for i, step := range steps {
		got := s.save(t, step.change)
		if want := (SaveResult{Changed: step.want}); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Save = %+v, want %+v", step.name, got, want)
		}
		if got := s.stored(t, "testmonitor").Revision; got != int64(i+2) {
			t.Errorf("%s: revision %d, want %d", step.name, got, i+2)
		}
	}
	sameSaved(t, "at the end", s.stored(t, "testmonitor"), Saved{})
	if s.checker.callCount() != 0 || s.supervisor.restartCount() != 0 {
		t.Errorf("%d validations and %d restarts, want none", s.checker.callCount(), s.supervisor.restartCount())
	}
	records := logRecords(t, s.log)
	last := map[string]any{"level": "INFO", "msg": "monitor configuration saved", "monitor": "testmonitor", "changed": []any{"ignored:older"}, "restarted": false}
	if len(records) != len(steps) || !reflect.DeepEqual(records[len(records)-1], last) {
		t.Errorf("log records = %v, want %d ending with %v", records, len(steps), last)
	}
}

func TestANullSecretClearsThePlainValueSavedBeforeTheFieldBecameSecret(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.put(t, "testmonitor", Saved{Document: raws(t, `{"db":"plain-old-value","n":6}`)})

	result := s.save(t, Change{Secrets: map[string]*string{"db": nil}})

	if want := (SaveResult{Changed: []string{"db"}, Restarted: true, Request: 1}); !reflect.DeepEqual(result, want) {
		t.Errorf("Save = %+v, want %+v", result, want)
	}
	sameSaved(t, "after the save", s.stored(t, "testmonitor"), Saved{Document: raws(t, `{"n":6}`)})
	if _, ok := decodeDocument(t, s.checker.document)["db"]; ok {
		t.Error("the checked document still holds db")
	}
	view := readView(t, s.service, "testmonitor")
	i := slices.IndexFunc(view.Secrets, func(st SecretState) bool { return st.Path == "db" })
	if i < 0 || view.Secrets[i] != (SecretState{Path: "db"}) {
		t.Errorf("secret states = %+v, want db neither set nor saved", view.Secrets)
	}
}

func TestSaveNamesOnlyWhatItChanges(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.put(t, "testmonitor", Saved{
		Document:  raws(t, `{"n":7}`),
		Secrets:   map[string]string{"db": "row-db-secret-value"},
		Variables: map[string]Variable{"FREE": {Value: "f"}, "KEY": {Value: "k", Secret: true}},
	})

	result := s.save(t, Change{
		Document: raws(t, `{"n":7,"mode":"full"}`),
		Secrets:  map[string]*string{"db": text("row-db-secret-value"), "pin": nil, "level": text("low")},
		Variables: VariableChange{
			Set:   map[string]string{"FREE": "f", "KEY": "k"},
			Unset: []string{"GONE"},
		},
	})

	if want := []string{"mode", "level", "env:KEY"}; !slices.Equal(result.Changed, want) {
		t.Errorf("Changed = %v, want %v", result.Changed, want)
	}
}

func TestASaveWhileTheLastRestartIsUnsettledIs409(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	n := func(value string) Change { return Change{Document: raws(t, `{"n":`+value+`}`)} }

	if got := s.save(t, n("1")); got.Request != 1 {
		t.Fatalf("first save requested %d", got.Request)
	}
	s.supervisor.setAttempt(supervisor.Attempt{Request: 1, Phase: supervisor.PhaseStarting})
	if _, err := s.service.Save(t.Context(), "testmonitor", n("2")); !errors.Is(err, ErrRestarting) {
		t.Errorf("Save while starting = %v, want ErrRestarting", err)
	}
	// The back-off starting again after a failure has settled the request.
	s.supervisor.setAttempt(supervisor.Attempt{Request: 1, Phase: supervisor.PhaseStarting, Settled: 1})
	if got := s.save(t, n("2")); !got.Restarted || got.Request != 2 {
		t.Errorf("Save after the request settled = %+v, want restart 2", got)
	}
	s.supervisor.setAttempt(supervisor.Attempt{Request: 2, Phase: supervisor.PhaseStarting, Settled: 1})
	if _, err := s.service.Save(t.Context(), "testmonitor", n("3")); !errors.Is(err, ErrRestarting) {
		t.Errorf("Save while the second restart starts = %v, want ErrRestarting", err)
	}
	s.clock.set(startedAt.Add(75 * time.Second))
	if got := s.save(t, n("3")); !got.Restarted || got.Request != 3 {
		t.Errorf("Save once the monitor gives no answer = %+v, want restart 3", got)
	}
	if got := s.stored(t, "testmonitor").Revision; got != 3 {
		t.Errorf("revision %d, want 3", got)
	}
}

func TestConcurrentSavesOneWins(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.checker.entered = make(chan struct{})
	s.checker.release = make(chan struct{})
	type outcome struct {
		result SaveResult
		err    error
	}
	first := make(chan outcome, 1)
	change := Change{Document: raws(t, `{"n":1}`)}
	go func() {
		result, err := s.service.Save(context.Background(), "testmonitor", change)
		first <- outcome{result, err}
	}()
	select {
	case <-s.checker.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first save never reached the checker")
	}

	if _, err := s.service.Save(t.Context(), "testmonitor", Change{Document: raws(t, `{"n":2}`)}); !errors.Is(err, ErrBusy) {
		t.Errorf("second Save = %v, want ErrBusy", err)
	}
	close(s.checker.release)
	got := <-first
	if got.err != nil || !got.result.Restarted {
		t.Errorf("first Save = %+v, %v; want a restart", got.result, got.err)
	}
	if n := s.supervisor.restartCount(); n != 1 {
		t.Errorf("%d restarts, want 1", n)
	}
	sameSaved(t, "after both", s.stored(t, "testmonitor"), Saved{Document: raws(t, `{"n":1}`)})
}

// racingStore writes over the stored revision right before each Put, as another hub would.
type racingStore struct {
	*Postgres
}

func (s racingStore) Put(ctx context.Context, monitor string, saved Saved, previous int64) (int64, error) {
	if _, err := s.Postgres.Put(ctx, monitor, Saved{Document: map[string]json.RawMessage{"n": json.RawMessage(`9`)}}, previous); err != nil {
		return 0, err
	}
	return s.Postgres.Put(ctx, monitor, saved, previous)
}

func TestAStaleRevisionIs409(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.put(t, "testmonitor", Saved{Document: raws(t, `{"n":6}`)})
	r := readView(t, s.service, "testmonitor").Revision
	s.save(t, Change{Document: raws(t, `{"n":7}`)})

	if _, err := s.service.Save(t.Context(), "testmonitor", Change{Document: raws(t, `{"n":8}`), Revision: revision(r)}); !errors.Is(err, ErrStale) {
		t.Errorf("Save with revision %d = %v, want ErrStale", r, err)
	}
	if got := s.save(t, Change{Document: raws(t, `{"n":8}`), Revision: revision(r + 1)}); !got.Restarted {
		t.Errorf("Save with the stored revision = %+v, want a restart", got)
	}
	sameSaved(t, "after the saves", s.stored(t, "testmonitor"), Saved{Document: raws(t, `{"n":8}`)})

	t.Run("a write between the read and the write", func(t *testing.T) {
		store := newStore(t)
		racing := newSaveSceneOn(t, racingStore{store}, saveFields, saveEnviron)
		if _, err := racing.service.Save(t.Context(), "testmonitor", Change{Document: raws(t, `{"n":1}`)}); !errors.Is(err, ErrStale) {
			t.Errorf("Save = %v, want ErrStale", err)
		}
		if n := racing.supervisor.restartCount(); n != 0 {
			t.Errorf("%d restarts", n)
		}
	})
}

func TestSecretsMergeByPath(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.put(t, "testmonitor", Saved{
		Document: raws(t, `{"mail":{"port":25}}`),
		Secrets:  map[string]string{"db": "first-db-secret", "mail.password": "first-mail-secret", "pin": "1234"},
	})

	result := s.save(t, Change{
		Document: raws(t, `{"mail":{"host":"smtp.example.test"}}`),
		Secrets:  map[string]*string{"pin": nil, "db": text("second-db-secret")},
	})

	if want := []string{"mail", "db", "pin"}; !slices.Equal(result.Changed, want) {
		t.Errorf("Changed = %v, want %v", result.Changed, want)
	}
	sameSaved(t, "after the save", s.stored(t, "testmonitor"), Saved{
		Document: raws(t, `{"mail":{"host":"smtp.example.test"}}`),
		Secrets:  map[string]string{"db": "second-db-secret", "mail.password": "first-mail-secret"},
	})
	doc := decodeDocument(t, s.checker.document)
	mail, _ := doc["mail"].(map[string]any)
	if mail["password"] != "first-mail-secret" || mail["host"] != "smtp.example.test" || doc["db"] != "second-db-secret" {
		t.Error("the checked document does not hold the merged values (values not shown)")
	}
	if _, ok := doc["pin"]; ok {
		t.Error("the checked document holds the deleted secret")
	}
}

func TestIgnoredValuesSurviveASave(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.put(t, "testmonitor", Saved{
		// old_field and old_secret belong to a declaration the monitor no longer ships.
		Document:  raws(t, `{"old_field":"kept-plain","n":6,"mail":{"port":25,"password":"old-mail-plain"}}`),
		Secrets:   map[string]string{"old_secret": "old-secret-value", "mail.password": "first-mail-secret"},
		Ignored:   []IgnoredValue{{Path: "pin", Value: json.RawMessage(`"plain-pin"`), Reason: ReasonType}},
		Variables: map[string]Variable{"PATH": {Value: "/y"}, "FREE": {Value: "f"}},
	})
	oldField := IgnoredValue{Path: "old_field", Value: json.RawMessage(`"kept-plain"`), Reason: ReasonUndeclared}
	oldSecret := IgnoredValue{Path: "old_secret", Value: json.RawMessage(`"old-secret-value"`), Secret: true, Reason: ReasonUndeclared}
	// pin is secret today, so the value stored as plain counts as secret.
	pin := IgnoredValue{Path: "pin", Value: json.RawMessage(`"plain-pin"`), Secret: true, Reason: ReasonType}
	replaced := IgnoredValue{Path: "mail.password", Value: json.RawMessage(`"old-mail-plain"`), Secret: true, Reason: ReasonReplaced}
	kept := Saved{
		Document:  raws(t, `{"n":6,"mail":{"port":25}}`),
		Secrets:   map[string]string{"mail.password": "first-mail-secret"},
		Variables: map[string]Variable{"FREE": {Value: "f"}},
	}

	steps := []struct {
		name    string
		change  Change
		changed []string
		ignored []IgnoredValue
	}{
		{"deleting a stored free variable", Change{DeleteIgnored: []string{"env:PATH"}}, []string{"ignored:env:PATH"}, []IgnoredValue{replaced, oldField, oldSecret, pin}},
		{"another save", Change{Document: raws(t, `{"mode":"full"}`)}, []string{"mode"}, []IgnoredValue{replaced, oldField, oldSecret, pin}},
		{"deleting one ignored value", Change{DeleteIgnored: []string{"old_field"}}, []string{"ignored:old_field"}, []IgnoredValue{replaced, oldSecret, pin}},
	}
	for _, step := range steps {
		result := s.save(t, step.change)
		if !slices.Equal(result.Changed, step.changed) {
			t.Errorf("%s: Changed = %v, want %v", step.name, result.Changed, step.changed)
		}
		if step.name == "another save" {
			kept.Document["mode"] = json.RawMessage(`"full"`)
		}
		want := kept
		want.Ignored = step.ignored
		sameSaved(t, step.name, s.stored(t, "testmonitor"), want)
	}
	if n := s.supervisor.restartCount(); n != 1 {
		t.Errorf("%d restarts, want 1 (only the save of mode)", n)
	}
}

func TestAPageValueOverridesBadEnvText(t *testing.T) {
	s := newSaveScene(t, integerFields, []string{"TESTMONITOR__N=three"})

	_, err := s.service.Save(t.Context(), "testmonitor", Change{Secrets: map[string]*string{"p": text("pw-secret-value")}})
	if got, want := problemsOf(t, err), []Problem{{"n", `N is not an integer ("three")`}}; !reflect.DeepEqual(got, want) {
		t.Errorf("problems = %s, want %v", maskedError(err), want)
	}
	if s.stored(t, "testmonitor").Revision != 0 || s.checker.callCount() != 0 {
		t.Error("a save the .env breaks was written or checked")
	}

	if got := s.save(t, Change{Document: raws(t, `{"n":3}`)}); !got.Restarted {
		t.Errorf("Save = %+v, want a restart", got)
	}
	if doc := decodeDocument(t, s.checker.document); doc["n"] != json.Number("3") {
		t.Errorf("the checked document holds n = %v, want 3", doc["n"])
	}
}

func TestSaveWithoutDeclarationOnlyDeletes(t *testing.T) {
	s := newSaveScene(t, saveFields, saveEnviron)
	s.put(t, "bare", Saved{
		Document: raws(t, `{"n":7,"m":8}`),
		Secrets:  map[string]string{"p": "row-db-secret-value"},
		Ignored:  []IgnoredValue{{Path: "older", Value: json.RawMessage(`"kept"`), Reason: ReasonUndeclared}},
	})

	result, err := s.service.Save(t.Context(), "bare", Change{DeleteIgnored: []string{"n", "older"}})
	if err != nil {
		t.Fatalf("Save: %s", maskedError(err))
	}
	if want := (SaveResult{Changed: []string{"ignored:n", "ignored:older"}}); !reflect.DeepEqual(result, want) {
		t.Errorf("Save = %+v, want %+v", result, want)
	}
	// The other values wait, where they are, for a declaration that takes them.
	sameSaved(t, "bare", s.stored(t, "bare"), Saved{Document: raws(t, `{"m":8}`), Secrets: map[string]string{"p": "row-db-secret-value"}})
	if s.checker.callCount() != 0 || s.supervisor.restartCount() != 0 {
		t.Error("a save without a declaration was checked or restarted the monitor")
	}
}

// putFails reads through Postgres and fails every write.
type putFails struct {
	*Postgres
}

func (putFails) Put(context.Context, string, Saved, int64) (int64, error) {
	return 0, errors.New("dial tcp 127.0.0.1:5432: connection refused")
}

func TestSaveOfAnUnknownMonitorAndADownDatabase(t *testing.T) {
	down := &flakyStore{down: true}
	s := newSaveSceneOn(t, down, saveFields, saveEnviron)
	if _, err := s.service.Save(t.Context(), "nope", Change{}); !errors.Is(err, ErrUnknown) {
		t.Errorf("Save of an unknown monitor = %v, want ErrUnknown", err)
	}
	if down.gets != 0 {
		t.Errorf("Save of an unknown monitor read the database %d times", down.gets)
	}
	if _, err := s.service.Save(t.Context(), "testmonitor", Change{Document: raws(t, `{"n":1}`)}); !errors.Is(err, ErrDatabase) {
		t.Errorf("Save with the database down = %v, want ErrDatabase", err)
	}

	failing := newSaveSceneOn(t, putFails{newStore(t)}, saveFields, saveEnviron)
	if _, err := failing.service.Save(t.Context(), "testmonitor", Change{Document: raws(t, `{"n":1}`)}); !errors.Is(err, ErrDatabase) {
		t.Errorf("Save whose write fails = %v, want ErrDatabase", err)
	}
	if n := failing.supervisor.restartCount(); n != 0 {
		t.Errorf("%d restarts after a failed write", n)
	}
}
