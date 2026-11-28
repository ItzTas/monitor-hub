package monitorconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

type FieldState struct {
	Name   string
	Source Source
	Saved  bool
	EnvSet bool
}

type SecretState struct {
	Path   string
	Set    bool
	Saved  bool
	EnvSet bool
}

type VariableView struct {
	Name   string
	Secret bool
	Source Source
	Value  *string
	Saved  bool
	EnvSet bool
}

type IgnoredView struct {
	Path   string
	Value  json.RawMessage
	Secret bool
	Reason IgnoredReason
}

// View is a monitor's configuration as the API shows it: never a secret value.
type View struct {
	Monitor     string
	Declared    bool
	Declaration *settings.Declaration
	Revision    int64
	Document    map[string]any
	Fields      []FieldState
	Secrets     []SecretState
	Ignored     []IgnoredView
	Variables   []VariableView
	Problems    []Problem
}

func (s *Service) Read(ctx context.Context, name string) (View, error) {
	entry, ok := s.entries[name]
	if !ok {
		return View{}, ErrUnknown
	}
	saved, a, err := s.load(ctx, entry)
	if err != nil {
		return View{}, errors.Join(ErrDatabase, err)
	}
	view := View{Monitor: name, Revision: saved.Revision, Ignored: ignoredViews(entry.Settings, a.Ignored)}
	decl := entry.Settings
	if decl == nil {
		return view, nil
	}
	view.Declared = true
	view.Declaration = decl
	view.Document = shownValues(decl.Fields, a.Values, true)
	view.Problems = a.Problems
	for _, f := range decl.Fields {
		view.Fields = append(view.Fields, FieldState{
			Name:   f.Name,
			Source: a.Sources[f.Name],
			Saved:  f.Editable && holds(saved, f, f.Name),
			EnvSet: a.EnvSet[f.Name],
		})
		view.Secrets = append(view.Secrets, secretStates(f, f.Name, a.Values[f.Name], f.Editable, saved, a.EnvSet[f.Name])...)
	}
	view.Variables = s.variables(entry.Manifest.Name, decl, saved)
	return view, nil
}

// shownValues copies values field by field, leaving out secret fields and, at the top level,
// fields that are not editable.
func shownValues(fields []settings.Field, values map[string]any, top bool) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		value, ok := values[f.Name]
		if !ok || f.Secret || top && !f.Editable {
			continue
		}
		if object, isObject := value.(map[string]any); isObject && f.Type == settings.Object {
			value = shownValues(f.Fields, object, false)
		}
		out[f.Name] = value
	}
	return out
}

// holds reports whether a value is saved for f: a secret by its path, the rest by top-level name.
func holds(saved Saved, f settings.Field, path string) bool {
	if f.Secret {
		_, ok := saved.Secrets[path]
		return ok
	}
	_, ok := saved.Document[path]
	return ok
}

// secretStates lists the secret paths at and under f. Only top-level fields have an env.
func secretStates(f settings.Field, path string, value any, editable bool, saved Saved, envSet bool) []SecretState {
	if f.Secret {
		return []SecretState{{Path: path, Set: value != nil && value != "", Saved: editable && holds(saved, f, path), EnvSet: envSet}}
	}
	if f.Type != settings.Object {
		return nil
	}
	object, _ := value.(map[string]any)
	var states []SecretState
	for _, child := range f.Fields {
		states = append(states, secretStates(child, path+"."+child.Name, object[child.Name], editable, saved, false)...)
	}
	return states
}

// ignoredViews hides the value of what was secret when saved or is secret today.
func ignoredViews(decl *settings.Declaration, ignored []IgnoredValue) []IgnoredView {
	var views []IgnoredView
	for _, v := range ignored {
		view := IgnoredView{Path: v.Path, Secret: v.Secret, Reason: v.Reason}
		if !v.Secret && !secretToday(decl, v.Path) {
			view.Value = bytes.Clone(v.Value)
		}
		views = append(views, view)
	}
	return views
}

func secretToday(decl *settings.Declaration, path string) bool {
	if decl == nil {
		return false
	}
	if env, ok := strings.CutPrefix(path, "env:"); ok {
		return slices.ContainsFunc(decl.Fields, func(f settings.Field) bool { return f.Secret && f.Env == env })
	}
	fields := decl.Fields
	names := strings.Split(path, ".")
	for i, name := range names {
		j := slices.IndexFunc(fields, func(f settings.Field) bool { return f.Name == name })
		if j < 0 {
			return false
		}
		if i == len(names)-1 {
			return fields[j].Secret
		}
		fields = fields[j].Fields
	}
	return false
}

// variables are the free variables that reach the monitor: the saved ones, and the prefixed
// ones of the .env no field declares, which the hub cannot tell are not secret.
func (s *Service) variables(name string, decl *settings.Declaration, saved Saved) []VariableView {
	declared := map[string]bool{}
	for _, f := range decl.Fields {
		if f.Env != "" {
			declared[f.Env] = true
		}
	}
	reaches := func(key string) bool { return !settings.Reserved(key) && !declared[key] }
	env := envTexts(name, s.deps.Environ)
	byName := map[string]VariableView{}
	for key, v := range saved.Variables {
		if !catalog.VariableName(key) || !reaches(key) {
			continue
		}
		view := VariableView{Name: key, Secret: v.Secret, Source: SourcePage, Saved: true, EnvSet: env[key] != ""}
		if !v.Secret {
			view.Value = &v.Value
		}
		byName[key] = view
	}
	for key, text := range env {
		if _, ok := byName[key]; ok || key == "" || !reaches(key) {
			continue
		}
		byName[key] = VariableView{Name: key, Secret: true, Source: SourceEnv, EnvSet: text != ""}
	}
	var views []VariableView
	for _, key := range slices.Sorted(maps.Keys(byName)) {
		views = append(views, byName[key])
	}
	return views
}
