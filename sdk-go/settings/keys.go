package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// role is what a JSON value is in settings.json.
type role int

const (
	roleOther role = iota
	roleTop
	roleField
	roleItem
	roleFieldList
)

var attributes = map[role][]string{
	roleTop:   {"settings_version", "fields"},
	roleField: {"name", "label", "help", "type", "required", "secret", "editable", "reason", "group", "env", "options", "default", "fields", "items"},
	roleItem:  {"type", "options", "fields"},
}

type frame struct {
	role       role
	object     bool
	keys       map[string]bool
	key        string
	expectsKey bool
}

// next is the role of the value that comes next inside f.
func (f *frame) next() role {
	if !f.object {
		if f.role == roleFieldList {
			return roleField
		}
		return roleOther
	}
	if f.key == "fields" && f.role != roleOther {
		return roleFieldList
	}
	if f.key == "items" && f.role == roleField {
		return roleItem
	}
	return roleOther
}

func (f *frame) addKey(key string) string {
	if f.keys[key] {
		return at("", fmt.Sprintf("key %q appears more than once in one object", key))
	}
	f.keys[key], f.key, f.expectsKey = true, key, false
	for _, known := range attributes[f.role] {
		if key != known && strings.EqualFold(key, known) {
			return at("", fmt.Sprintf("key %q differs from %q only in letter case", key, known))
		}
	}
	return ""
}

// scanKeys finds the repeated keys and the attributes written in another letter case, which a map
// would hide. It reads only the first JSON value and leaves bad JSON to the decoder that follows.
func scanKeys(data []byte) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		var parent *frame
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		if key, ok := tok.(string); ok && parent != nil && parent.expectsKey {
			if problem := parent.addKey(key); problem != "" {
				return problem
			}
			continue
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			r := roleTop
			if parent != nil {
				r = parent.next()
			}
			object := tok == json.Delim('{')
			// Only an object can be the top, a field or an item, and only an array a list of fields.
			if object == (r == roleFieldList) {
				r = roleOther
			}
			stack = append(stack, &frame{role: r, object: object, keys: map[string]bool{}, expectsKey: object})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			return ""
		}
		stack[len(stack)-1].expectsKey = stack[len(stack)-1].object
	}
}
