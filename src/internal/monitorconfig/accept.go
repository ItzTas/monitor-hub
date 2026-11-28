package monitorconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

type IgnoredReason string

const (
	ReasonUndeclared    IgnoredReason = "undeclared"
	ReasonType          IgnoredReason = "type"
	ReasonNotEditable   IgnoredReason = "not_editable"
	ReasonNoDeclaration IgnoredReason = "no_declaration"
	// A newer value replaced it.
	ReasonReplaced IgnoredReason = "replaced"
)

type IgnoredValue struct {
	Path   string          `json:"path"`
	Value  json.RawMessage `json:"value"`
	Secret bool            `json:"secret"`
	Reason IgnoredReason   `json:"reason"`
}

// Accept returns the part of a saved value that field takes, or nil when it takes none of it,
// and what it left out. null is refused at any depth.
func Accept(field settings.Field, path string, raw json.RawMessage) (any, []IgnoredValue) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, []IgnoredValue{{Path: path, Secret: field.Secret, Reason: ReasonType}}
	}
	if accepted, ignored, ok := accept(field, path, value); ok {
		return accepted, ignored
	}
	return nil, []IgnoredValue{refused(field, path, value)}
}

// accept reports false when field refuses value whole; the caller records that.
func accept(field settings.Field, path string, value any) (any, []IgnoredValue, bool) {
	switch field.Type {
	case settings.List:
		return acceptList(field, path, value)
	case settings.Object:
		return acceptObject(field, path, value)
	}
	return value, nil, fits(field, value)
}

func fits(field settings.Field, value any) bool {
	text, isText := value.(string)
	number, isNumber := value.(json.Number)
	_, isBoolean := value.(bool)
	checks := map[settings.Type]func() bool{
		settings.String:   func() bool { return isText },
		settings.Duration: func() bool { return isText },
		settings.Choice:   func() bool { return isText && slices.Contains(field.Options, text) },
		settings.Boolean:  func() bool { return isBoolean },
		// serde_json reads -0 as a float, so the monitor would refuse it.
		settings.Integer: func() bool {
			_, err := strconv.ParseInt(string(number), 10, 64)
			return isNumber && err == nil && number != "-0"
		},
		settings.Number: func() bool {
			_, err := strconv.ParseFloat(string(number), 64)
			return isNumber && err == nil
		},
	}
	return checks[field.Type]()
}

func acceptList(field settings.Field, path string, value any) (any, []IgnoredValue, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, nil, false
	}
	item := itemField(field)
	kept := []any{}
	var ignored []IgnoredValue
	for i, v := range items {
		at := fmt.Sprintf("%s[%d]", path, i)
		accepted, left, ok := accept(item, at, v)
		ignored = append(ignored, left...)
		if !ok {
			ignored = append(ignored, refused(item, at, v))
			continue
		}
		kept = append(kept, accepted)
	}
	return kept, ignored, true
}

// itemField is a list item checked as a field.
func itemField(list settings.Field) settings.Field {
	return settings.Field{Name: list.Name, Type: list.Items.Type, Options: list.Items.Options, Fields: list.Items.Fields}
}

func acceptObject(field settings.Field, path string, value any) (any, []IgnoredValue, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, nil, false
	}
	kept := map[string]any{}
	var ignored []IgnoredValue
	for _, key := range slices.Sorted(maps.Keys(object)) {
		at := path + "." + key
		i := slices.IndexFunc(field.Fields, func(f settings.Field) bool { return f.Name == key })
		if i < 0 {
			ignored = append(ignored, IgnoredValue{Path: at, Value: encode(object[key]), Reason: ReasonUndeclared})
			continue
		}
		accepted, left, ok := accept(field.Fields[i], at, object[key])
		ignored = append(ignored, left...)
		if !ok {
			ignored = append(ignored, refused(field.Fields[i], at, object[key]))
			continue
		}
		kept[key] = accepted
	}
	return kept, ignored, true
}

func refused(field settings.Field, path string, value any) IgnoredValue {
	return IgnoredValue{Path: path, Value: encode(value), Secret: field.Secret, Reason: ReasonType}
}

// encode writes a decoded JSON value back, which cannot fail.
func encode(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
