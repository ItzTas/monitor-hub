package monitorconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

const maxValueBytes = 8192

var (
	ErrBusy       = errors.New("another save of this monitor is in progress")
	ErrRestarting = errors.New("the monitor is still restarting after the last save")
)

type VariableChange struct {
	Set    map[string]string
	Unset  []string
	Secret []string
}

// Change merges over what is saved, field by field: a null in Document or a nil in Secrets
// goes back to the .env text or the default, and what it leaves out stays as it is.
type Change struct {
	Document      map[string]json.RawMessage
	Secrets       map[string]*string
	Variables     VariableChange
	DeleteIgnored []string
	Revision      *int64
}

// SaveResult names what the save changed, never a value.
type SaveResult struct {
	Changed   []string
	Restarted bool
	Request   uint64
}

type ProblemsError struct {
	Problems []Problem
}

func (e *ProblemsError) Error() string {
	return "the values have problems"
}

// Save checks the change, has the monitor validate the document it makes, stores it and
// restarts the monitor when its document or environment changes.
func (s *Service) Save(ctx context.Context, name string, change Change) (SaveResult, error) {
	entry, ok := s.entries[name]
	if !ok {
		return SaveResult{}, ErrUnknown
	}
	lock := s.locks[name]
	if !lock.TryLock() {
		return SaveResult{}, ErrBusy
	}
	defer lock.Unlock()
	if s.progress(ctx, name).State == ProgressRestarting {
		return SaveResult{}, ErrRestarting
	}
	saved, before, err := s.load(ctx, entry)
	if err != nil {
		return SaveResult{}, errors.Join(ErrDatabase, err)
	}
	if change.Revision != nil && *change.Revision != saved.Revision {
		return SaveResult{}, ErrStale
	}
	if problems := hubProblems(entry.Settings, before, change); len(problems) > 0 {
		return SaveResult{}, &ProblemsError{problems}
	}
	next := s.next(entry, saved, change)
	if unchanged(saved, next) {
		return SaveResult{}, nil
	}
	changed := changedNames(entry.Settings, saved, change)
	after := Assemble(entry, s.deps.Environ, s.deps.DataRoot, next)
	if len(after.Problems) > 0 {
		return SaveResult{}, &ProblemsError{after.Problems}
	}
	if bytes.Equal(before.Document, after.Document) && slices.Equal(before.Env, after.Env) {
		if err := s.put(ctx, name, next, saved.Revision); err != nil {
			return SaveResult{}, err
		}
		s.deps.Log.Info("monitor configuration saved", "monitor", name, "changed", changed, "restarted", false)
		return SaveResult{Changed: changed}, nil
	}
	if err := s.validate(ctx, entry, after, slices.Concat(before.Secrets, after.Secrets)); err != nil {
		return SaveResult{}, err
	}
	if err := s.put(ctx, name, next, saved.Revision); err != nil {
		return SaveResult{}, err
	}
	number, err := s.deps.Supervisor.Restart(name)
	if err != nil {
		return SaveResult{}, fmt.Errorf("restart monitor %s: %w", name, err)
	}
	s.mu.Lock()
	s.requests[name] = request{number: number, at: s.deps.Now()}
	s.mu.Unlock()
	s.deps.Log.Info("monitor configuration saved", "monitor", name, "changed", changed, "restarted", true)
	return SaveResult{Changed: changed, Restarted: true, Request: number}, nil
}

// validate runs the monitor's validate-settings; what it says leaves out the secrets.
func (s *Service) validate(ctx context.Context, entry catalog.Entry, after Assembly, secrets []string) error {
	problems, err := s.deps.Checker.Validate(ctx, entry, after.Env, after.Document)
	var validatorErr *ValidatorError
	if errors.As(err, &validatorErr) {
		reason, stderr := redact(validatorErr.Reason, secrets), redact(validatorErr.Stderr, secrets)
		s.deps.Log.Warn("the monitor could not validate its configuration", "monitor", entry.Manifest.Name, "reason", reason, "stderr", stderr)
		return &ValidatorError{Reason: reason, Stderr: stderr}
	}
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		return nil
	}
	redacted := make([]Problem, len(problems))
	for i, p := range problems {
		redacted[i] = Problem{Path: p.Path, Message: redact(p.Message, secrets)}
	}
	return &ProblemsError{redacted}
}

func (s *Service) put(ctx context.Context, name string, next Saved, previous int64) error {
	next.UpdatedAt = s.deps.Now()
	_, err := s.deps.Store.Put(ctx, name, next, previous)
	if errors.Is(err, ErrStale) {
		return ErrStale
	}
	if err != nil {
		return errors.Join(ErrDatabase, err)
	}
	return nil
}

// next is what the save stores. With a declaration, the values it does not take move to the
// ignored ones; without one, they stay where they are until a declaration takes them.
func (s *Service) next(entry catalog.Entry, saved Saved, change Change) Saved {
	deleted := func(v IgnoredValue) bool { return slices.Contains(change.DeleteIgnored, v.Path) }
	if entry.Settings == nil {
		return deletedWithoutDeclaration(saved, change.DeleteIgnored)
	}
	a := Assemble(entry, s.deps.Environ, s.deps.DataRoot, merged(entry.Settings, saved, change))
	next := a.kept
	for _, v := range slices.DeleteFunc(a.Ignored, deleted) {
		v.Secret = v.Secret || secretToday(entry.Settings, v.Path)
		next.Ignored = append(next.Ignored, v)
	}
	return next
}

func merged(decl *settings.Declaration, saved Saved, change Change) Saved {
	next := Saved{
		Document:  orEmpty(maps.Clone(saved.Document)),
		Secrets:   orEmpty(maps.Clone(saved.Secrets)),
		Ignored:   slices.Clone(saved.Ignored),
		Variables: orEmpty(maps.Clone(saved.Variables)),
	}
	for key, raw := range change.Document {
		next.Document[key] = raw
		if isNull(raw) {
			delete(next.Document, key)
		}
	}
	for path, text := range change.Secrets {
		if text == nil {
			delete(next.Secrets, path)
			if topSecret(decl, path) {
				delete(next.Document, path)
			}
			continue
		}
		next.Secrets[path] = *text
	}
	for _, key := range change.Variables.Unset {
		delete(next.Variables, key)
	}
	for key, value := range change.Variables.Set {
		next.Variables[key] = Variable{Value: value, Secret: slices.Contains(change.Variables.Secret, key)}
	}
	return next
}

// deletedWithoutDeclaration removes each path from wherever the ignored values of a monitor
// without a declaration come from.
func deletedWithoutDeclaration(saved Saved, paths []string) Saved {
	next := clone(saved)
	for _, path := range paths {
		delete(next.Document, path)
		delete(next.Secrets, path)
		if key, ok := strings.CutPrefix(path, "env:"); ok {
			delete(next.Variables, key)
		}
	}
	next.Ignored = slices.DeleteFunc(next.Ignored, func(v IgnoredValue) bool { return slices.Contains(paths, v.Path) })
	return next
}

// topSecret reports whether path is a top-level secret field. A null secret there also clears
// the plain value saved before the field became secret.
func topSecret(decl *settings.Declaration, path string) bool {
	return decl != nil && slices.ContainsFunc(decl.Fields, func(f settings.Field) bool { return f.Name == path && f.Secret })
}

// isNull takes both forms a JSON null reaches Change.Document in.
func isNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || string(trimmed) == "null"
}

func sameJSON(a, b json.RawMessage) bool {
	return reflect.DeepEqual(decodeDefault(a), decodeDefault(b))
}

func unchanged(a, b Saved) bool {
	return maps.EqualFunc(a.Document, b.Document, sameJSON) &&
		maps.Equal(a.Secrets, b.Secrets) &&
		maps.Equal(a.Variables, b.Variables) &&
		slices.EqualFunc(a.Ignored, b.Ignored, func(x, y IgnoredValue) bool {
			return x.Path == y.Path && x.Secret == y.Secret && x.Reason == y.Reason && sameJSON(x.Value, y.Value)
		})
}

// changedNames lists what the change sets differently from what is saved: top-level fields,
// secret paths, env:<NAME> and ignored:<path>, each group in order.
func changedNames(decl *settings.Declaration, saved Saved, change Change) []string {
	var changed []string
	for _, key := range slices.Sorted(maps.Keys(change.Document)) {
		old, had := saved.Document[key]
		raw := change.Document[key]
		if isNull(raw) && had || !isNull(raw) && (!had || !sameJSON(old, raw)) {
			changed = append(changed, key)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(change.Secrets)) {
		old, had := saved.Secrets[path]
		_, plain := saved.Document[path]
		text := change.Secrets[path]
		if text == nil && (had || plain && topSecret(decl, path)) || text != nil && (!had || old != *text) {
			changed = append(changed, path)
		}
	}
	names := slices.Concat(slices.Collect(maps.Keys(change.Variables.Set)), change.Variables.Unset)
	for _, key := range slices.Compact(slices.Sorted(slices.Values(names))) {
		old, had := saved.Variables[key]
		value, set := change.Variables.Set[key]
		if set && old != (Variable{Value: value, Secret: slices.Contains(change.Variables.Secret, key)}) || !set && had {
			changed = append(changed, "env:"+key)
		}
	}
	for _, path := range slices.Compact(slices.Sorted(slices.Values(change.DeleteIgnored))) {
		changed = append(changed, "ignored:"+path)
	}
	return changed
}

// problems keeps the first problem found at each path.
type problems map[string]string

func (p problems) add(path, message string) {
	if _, ok := p[path]; !ok {
		p[path] = message
	}
}

func (p problems) sorted() []Problem {
	var out []Problem
	for _, path := range slices.Sorted(maps.Keys(p)) {
		out = append(out, Problem{Path: path, Message: p[path]})
	}
	return out
}

const (
	undeclaredField = "settings.json does not declare this field"
	notEditable     = "the monitor does not allow changing this field here"
)

// hubProblems are the problems the hub finds in a change on its own, before the monitor sees it.
func hubProblems(decl *settings.Declaration, before Assembly, change Change) []Problem {
	found := problems{}
	ignored := map[string]bool{}
	for _, v := range before.Ignored {
		ignored[v.Path] = true
	}
	for _, path := range change.DeleteIgnored {
		if !ignored[path] {
			found.add("ignored:"+path, "no ignored value has this path")
		}
	}
	v := change.Variables
	if decl == nil {
		if len(change.Document)+len(change.Secrets)+len(v.Set)+len(v.Unset)+len(v.Secret) > 0 {
			found.add("", "the monitor ships no readable settings.json version 2, so its configuration cannot be changed here")
		}
		return found.sorted()
	}
	documentProblems(decl, change.Document, found)
	secretProblems(decl, change.Secrets, found)
	variableProblems(decl, v, found)
	return found.sorted()
}

func documentProblems(decl *settings.Declaration, document map[string]json.RawMessage, found problems) {
	for key, raw := range document {
		i := slices.IndexFunc(decl.Fields, func(f settings.Field) bool { return f.Name == key })
		if i < 0 {
			found.add(key, undeclaredField)
			continue
		}
		f := decl.Fields[i]
		if !f.Editable {
			found.add(key, notEditable)
			continue
		}
		if f.Secret {
			found.add(key, "this field is secret: send it in secrets")
			continue
		}
		if isNull(raw) {
			continue
		}
		value := decodeDefault(raw)
		secretChildren(f, key, value, found)
		_, refused := Accept(f, key, raw)
		for _, r := range refused {
			message := undeclaredField
			if r.Reason != ReasonUndeclared {
				message = Expected(fieldAt(f, r.Path))
			}
			found.add(r.Path, message)
		}
		texts(value, key, found)
	}
}

// secretChildren finds the secret fields inside an object sent in document.
func secretChildren(f settings.Field, path string, value any, found problems) {
	object, ok := value.(map[string]any)
	if !ok || f.Type != settings.Object {
		return
	}
	for _, child := range f.Fields {
		v, ok := object[child.Name]
		if !ok {
			continue
		}
		at := path + "." + child.Name
		if child.Secret {
			found.add(at, "this field is secret: send it in secrets")
			continue
		}
		secretChildren(child, at, v, found)
	}
}

// fieldAt finds the field, or list item, that path names inside f. Accept only passes through
// declared fields and list items.
func fieldAt(f settings.Field, path string) settings.Field {
	rest := strings.TrimPrefix(path, f.Name)
	for rest != "" {
		if rest[0] == '[' {
			f = itemField(f)
			rest = rest[strings.IndexByte(rest, ']')+1:]
			continue
		}
		rest = rest[1:]
		end := strings.IndexAny(rest, ".[")
		if end < 0 {
			end = len(rest)
		}
		i := slices.IndexFunc(f.Fields, func(g settings.Field) bool { return g.Name == rest[:end] })
		if i < 0 {
			return f
		}
		f, rest = f.Fields[i], rest[end:]
	}
	return f
}

// texts checks every text in value, except under a path that already has a problem.
func texts(value any, path string, found problems) {
	if _, ok := found[path]; ok {
		return
	}
	switch v := value.(type) {
	case string:
		if message := textProblem(v); message != "" {
			found.add(path, message)
		}
	case map[string]any:
		for key, child := range v {
			texts(child, path+"."+key, found)
		}
	case []any:
		for i, item := range v {
			texts(item, fmt.Sprintf("%s[%d]", path, i), found)
		}
	}
}

func textProblem(text string) string {
	if strings.ContainsRune(text, 0) {
		return "the value holds a NUL character"
	}
	if len(text) > maxValueBytes {
		return fmt.Sprintf("the value is longer than %d bytes", maxValueBytes)
	}
	return ""
}

func secretProblems(decl *settings.Declaration, secrets map[string]*string, found problems) {
	for path, text := range secrets {
		top, f, ok := secretField(decl, path)
		if !ok {
			found.add(path, "settings.json declares no secret field at this path")
			continue
		}
		if !top.Editable {
			found.add(path, notEditable)
			continue
		}
		if text == nil {
			continue
		}
		if message := textProblem(*text); message != "" {
			found.add(path, message)
			continue
		}
		if _, ok := convertSecret(f, *text); !ok {
			found.add(path, Expected(f))
		}
	}
}

// secretField finds the secret field at path and the top-level field it is in.
func secretField(decl *settings.Declaration, path string) (settings.Field, settings.Field, bool) {
	for _, f := range decl.Fields {
		if f.Name == path {
			return f, f, f.Secret
		}
		if _, child, ok := secretChild(f, path); ok {
			return f, child, true
		}
	}
	return settings.Field{}, settings.Field{}, false
}

func variableProblems(decl *settings.Declaration, v VariableChange, found problems) {
	declared := map[string]bool{}
	for _, f := range decl.Fields {
		if f.Env != "" {
			declared[f.Env] = true
		}
	}
	badName := "the name must match ^[A-Z][A-Z0-9_]*$"
	for name, value := range v.Set {
		checks := []struct {
			fails   bool
			message string
		}{
			{slices.Contains(v.Unset, name), "set and unset name the same variable"},
			{!catalog.VariableName(name), badName},
			{settings.Reserved(name), "the hub reserves this name"},
			{declared[name], "this name is a field of the monitor; change the field instead"},
			{textProblem(value) != "", textProblem(value)},
		}
		for _, c := range checks {
			if c.fails {
				found.add("env:"+name, c.message)
				break
			}
		}
	}
	for _, name := range v.Unset {
		if !catalog.VariableName(name) {
			found.add("env:"+name, badName)
		}
	}
	for _, name := range v.Secret {
		// A declared name in Set already has its own problem.
		if _, set := v.Set[name]; !set {
			found.add("env:"+name, "secret only marks undeclared variables that this request sets")
		}
	}
}
