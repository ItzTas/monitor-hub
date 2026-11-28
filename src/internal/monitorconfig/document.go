package monitorconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
)

type Source string

const (
	SourcePage    Source = "page"
	SourceEnv     Source = "env"
	SourceDefault Source = "default"
	SourceUnset   Source = "unset"
)

type Variable struct {
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

type Saved struct {
	Document  map[string]json.RawMessage
	Secrets   map[string]string
	Ignored   []IgnoredValue
	Variables map[string]Variable
	Revision  int64
	UpdatedAt time.Time
}

// Assembly is what one start of a monitor receives, and why. Document and Values are nil for a
// monitor without a declaration; Secrets are the texts to redact from what the monitor says.
type Assembly struct {
	Document []byte
	Values   map[string]any
	Sources  map[string]Source
	EnvSet   map[string]bool
	Ignored  []IgnoredValue
	Problems []Problem
	Env      []string
	Secrets  []string
}

var variablePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Assemble builds the document of a monitor field by field: the saved value, else the .env text,
// else the default.
func Assemble(entry catalog.Entry, environ []string, dataRoot string, saved Saved) Assembly {
	name := entry.Manifest.Name
	if entry.Settings == nil {
		return Assembly{
			Env:     catalog.Environment(name, environ, dataRoot, catalog.Layer{}),
			Ignored: withoutDeclaration(saved),
		}
	}
	a := assembler{
		decl:        entry.Settings,
		saved:       saved,
		env:         envTexts(name, environ),
		usedDoc:     map[string]bool{},
		usedSecrets: map[string]bool{},
		out:         Assembly{Sources: map[string]Source{}, EnvSet: map[string]bool{}},
	}
	doc := map[string]any{}
	for _, f := range a.decl.Fields {
		a.field(f, doc)
	}
	a.undeclared()
	a.out.Ignored = append(a.out.Ignored, saved.Ignored...)
	a.out.Values = WithDefaults(a.decl, doc)
	for _, f := range a.decl.Fields {
		if _, ok := a.out.Sources[f.Name]; ok {
			continue
		}
		a.out.Sources[f.Name] = SourceUnset
		if _, ok := doc[f.Name]; ok {
			a.out.Sources[f.Name] = SourceDefault
		}
	}
	a.out.Document = encode(doc)
	if len(a.out.Document) > settings.DocumentLimit {
		message := fmt.Sprintf("the configuration document is larger than %d bytes", settings.DocumentLimit)
		a.out.Problems = append(a.out.Problems, Problem{"", message})
	}
	a.out.Env = catalog.Environment(name, environ, dataRoot, a.layer())
	a.out.Secrets = a.secrets(a.decl.Fields, doc)
	for _, key := range slices.Sorted(maps.Keys(saved.Variables)) {
		if v := saved.Variables[key]; v.Secret && v.Value != "" {
			a.out.Secrets = append(a.out.Secrets, v.Value)
		}
	}
	return a.out
}

// envTexts holds the monitor's prefixed variables without the prefix; the last of a repeated
// name wins, as in catalog.Environment.
func envTexts(name string, environ []string) map[string]string {
	prefix := catalog.EnvPrefix(name)
	texts := map[string]string{}
	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if stripped, found := strings.CutPrefix(key, prefix); ok && found {
			texts[stripped] = value
		}
	}
	return texts
}

func withoutDeclaration(saved Saved) []IgnoredValue {
	var ignored []IgnoredValue
	for _, key := range slices.Sorted(maps.Keys(saved.Document)) {
		ignored = append(ignored, IgnoredValue{key, saved.Document[key], false, ReasonNoDeclaration})
	}
	for _, key := range slices.Sorted(maps.Keys(saved.Secrets)) {
		ignored = append(ignored, IgnoredValue{key, encode(saved.Secrets[key]), true, ReasonNoDeclaration})
	}
	for _, key := range slices.Sorted(maps.Keys(saved.Variables)) {
		v := saved.Variables[key]
		ignored = append(ignored, IgnoredValue{"env:" + key, encode(v.Value), v.Secret, ReasonNoDeclaration})
	}
	return append(ignored, saved.Ignored...)
}

type assembler struct {
	decl        *settings.Declaration
	saved       Saved
	env         map[string]string
	usedDoc     map[string]bool
	usedSecrets map[string]bool
	out         Assembly
}

func (a *assembler) ignore(path string, value json.RawMessage, secret bool, reason IgnoredReason) {
	a.out.Ignored = append(a.out.Ignored, IgnoredValue{path, value, secret, reason})
}

func (a *assembler) field(f settings.Field, doc map[string]any) {
	text := ""
	if f.Env != "" {
		text = a.env[f.Env]
	}
	if text != "" {
		a.out.EnvSet[f.Name] = true
	}
	if value, ok := a.candidate(f); ok {
		doc[f.Name] = value
		a.out.Sources[f.Name] = SourcePage
		return
	}
	if text == "" {
		return
	}
	a.out.Sources[f.Name] = SourceEnv
	values, problems := FromEnv(&settings.Declaration{Fields: []settings.Field{f}}, func(string) (string, bool) { return text, true })
	a.out.Problems = append(a.out.Problems, problems...)
	if value, ok := values[f.Name]; ok {
		doc[f.Name] = value
	}
}

// candidate is the saved value of a top-level field, when it has one the declaration takes.
func (a *assembler) candidate(f settings.Field) (any, bool) {
	if !f.Editable {
		a.notEditable(f)
		return nil, false
	}
	if text, ok := a.saved.Secrets[f.Name]; ok && f.Secret {
		a.usedSecrets[f.Name] = true
		return a.secretValue(f, f.Name, text)
	}
	raw, ok := a.saved.Document[f.Name]
	if ok {
		a.usedDoc[f.Name] = true
	}
	if f.Type == settings.Object {
		return a.object(f, raw, ok)
	}
	if !ok {
		return nil, false
	}
	return a.accept(f, raw)
}

func (a *assembler) notEditable(f settings.Field) {
	if raw, ok := a.saved.Document[f.Name]; ok {
		a.usedDoc[f.Name] = true
		a.ignore(f.Name, raw, f.Secret, ReasonNotEditable)
	}
	if text, ok := a.saved.Secrets[f.Name]; ok {
		a.usedSecrets[f.Name] = true
		a.ignore(f.Name, encode(text), true, ReasonNotEditable)
	}
}

func (a *assembler) accept(f settings.Field, raw json.RawMessage) (any, bool) {
	value, ignored := Accept(f, f.Name, raw)
	a.out.Ignored = append(a.out.Ignored, ignored...)
	return value, value != nil
}

// secretValue converts the saved text of a secret to the field's type.
func (a *assembler) secretValue(f settings.Field, path, text string) (any, bool) {
	value, expected := ConvertText(f.Type, text)
	if expected != "" || f.Type == settings.Choice && !slices.Contains(f.Options, text) {
		a.ignore(path, encode(text), true, ReasonType)
		return nil, false
	}
	return value, true
}

// object is the saved object with the saved secrets inside it put in place.
func (a *assembler) object(f settings.Field, raw json.RawMessage, saved bool) (any, bool) {
	var object map[string]any
	if saved {
		value, _ := a.accept(f, raw)
		object, _ = value.(map[string]any)
	}
	for _, path := range slices.Sorted(maps.Keys(a.saved.Secrets)) {
		names, child, ok := secretChild(f, path)
		if !ok {
			continue
		}
		a.usedSecrets[path] = true
		value, ok := a.secretValue(child, path, a.saved.Secrets[path])
		if !ok {
			continue
		}
		if object == nil {
			object = map[string]any{}
		}
		insert(object, names, value)
	}
	return object, object != nil
}

// secretChild finds the field that path names inside the object f, when the declaration marks it
// secret, and the names that lead to it from f.
func secretChild(f settings.Field, path string) ([]string, settings.Field, bool) {
	rest, ok := strings.CutPrefix(path, f.Name+".")
	if !ok {
		return nil, settings.Field{}, false
	}
	names := strings.Split(rest, ".")
	fields := f.Fields
	for i, name := range names {
		j := slices.IndexFunc(fields, func(g settings.Field) bool { return g.Name == name })
		if j < 0 {
			return nil, settings.Field{}, false
		}
		if i == len(names)-1 {
			return names, fields[j], fields[j].Secret
		}
		if fields[j].Type != settings.Object {
			return nil, settings.Field{}, false
		}
		fields = fields[j].Fields
	}
	return nil, settings.Field{}, false
}

func insert(object map[string]any, names []string, value any) {
	for _, name := range names[:len(names)-1] {
		next, ok := object[name].(map[string]any)
		if !ok {
			next = map[string]any{}
			object[name] = next
		}
		object = next
	}
	object[names[len(names)-1]] = value
}

func (a *assembler) undeclared() {
	for _, key := range slices.Sorted(maps.Keys(a.saved.Document)) {
		if !a.usedDoc[key] {
			a.ignore(key, a.saved.Document[key], false, ReasonUndeclared)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(a.saved.Secrets)) {
		if !a.usedSecrets[key] {
			a.ignore(key, encode(a.saved.Secrets[key]), true, ReasonUndeclared)
		}
	}
}

// layer drops the variables the fields declare and adds the valid free variables.
func (a *assembler) layer() catalog.Layer {
	var layer catalog.Layer
	for _, f := range a.decl.Fields {
		if f.Env != "" {
			layer.Drop = append(layer.Drop, f.Env)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(a.saved.Variables)) {
		v := a.saved.Variables[key]
		if !variablePattern.MatchString(key) || settings.Reserved(key) || slices.Contains(layer.Drop, key) {
			a.ignore("env:"+key, encode(v.Value), v.Secret, ReasonUndeclared)
			continue
		}
		if layer.Add == nil {
			layer.Add = map[string]string{}
		}
		layer.Add[key] = v.Value
	}
	return layer
}

// secrets are the non-empty texts at the secret paths of doc; a number counts by its digits.
func (a *assembler) secrets(fields []settings.Field, doc map[string]any) []string {
	var out []string
	for _, f := range fields {
		value := doc[f.Name]
		if f.Secret {
			if text := secretText(value); text != "" {
				out = append(out, text)
			}
		}
		if object, ok := value.(map[string]any); ok && f.Type == settings.Object {
			out = append(out, a.secrets(f.Fields, object)...)
		}
	}
	return out
}

func secretText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return string(v)
	}
	return ""
}

// WithDefaults fills doc as the SDK's with_defaults does: an absent field takes its default (an
// object one built from its fields' defaults, when not empty), and "" takes the default. null is
// kept.
func WithDefaults(decl *settings.Declaration, doc map[string]any) map[string]any {
	if doc == nil {
		doc = map[string]any{}
	}
	fill(decl.Fields, doc)
	return doc
}

func fill(fields []settings.Field, doc map[string]any) {
	for _, f := range fields {
		value, ok := doc[f.Name]
		if !ok {
			if v, ok := missing(f); ok {
				doc[f.Name] = v
			}
			continue
		}
		if value == "" && f.Default != nil {
			doc[f.Name] = decodeDefault(f.Default)
			continue
		}
		switch v := value.(type) {
		case map[string]any:
			fill(f.Fields, v)
		case []any:
			var children []settings.Field
			if f.Items != nil {
				children = f.Items.Fields
			}
			for _, item := range v {
				if object, ok := item.(map[string]any); ok {
					fill(children, object)
				}
			}
		}
	}
}

func missing(f settings.Field) (any, bool) {
	if f.Default != nil {
		return decodeDefault(f.Default), true
	}
	if f.Type != settings.Object {
		return nil, false
	}
	object := map[string]any{}
	fill(f.Fields, object)
	return object, len(object) > 0
}

// decodeDefault decodes a default that settings.Read already checked, a fresh copy each time.
func decodeDefault(raw json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	_ = dec.Decode(&value)
	return value
}
