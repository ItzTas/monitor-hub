package settings

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var byteOrderMark = []byte{0xEF, 0xBB, 0xBF}

// decoder stops at the first format problem. Strict, it refuses unknown attributes and types;
// lenient, it skips unknown attributes and reads an unknown type as a string.
type decoder struct {
	strict bool
	// written holds the path of each attribute read with a value, such as fields[1].env, because
	// some rules tell an absent attribute from an empty one and a Field cannot.
	written map[string]bool
}

func (d *decoder) decode(data []byte) (*Declaration, string) {
	if bytes.HasPrefix(data, byteOrderMark) {
		return nil, File + " starts with a UTF-8 byte order mark"
	}
	if !utf8.Valid(data) {
		return nil, notValid("", "the file is not UTF-8")
	}
	if problem := scanKeys(data); problem != "" {
		return nil, problem
	}
	d.written = map[string]bool{}
	o := d.object("", data, roleTop)
	if o.problem != "" {
		return nil, o.problem
	}
	if version := o.get("settings_version"); string(version) != strconv.Itoa(Version) {
		return nil, at("", fmt.Sprintf("settings_version is %s, want %d", cmp.Or(string(version), "missing"), Version))
	}
	if o.get("fields") == nil {
		return nil, at("", "fields is missing")
	}
	fields := o.fields()
	if o.problem != "" {
		return nil, o.problem
	}
	return &Declaration{Fields: fields}, ""
}

func (d *decoder) field(path string, raw json.RawMessage) (Field, string) {
	o := d.object(path, raw, roleField)
	f := Field{Editable: true}
	o.need("name", &f.Name)
	o.need("label", &f.Label)
	known := o.kind(&f.Type)
	o.flag("required", &f.Required)
	o.flag("secret", &f.Secret)
	o.flag("editable", &f.Editable)
	o.value("help", &f.Help)
	o.value("reason", &f.Reason)
	o.value("group", &f.Group)
	o.value("env", &f.Env)
	f.Default = o.get("default")
	if known {
		o.value("options", &f.Options)
		f.Fields = o.fields()
		f.Items = o.item()
	}
	return f, o.problem
}

func (d *decoder) item(path string, raw json.RawMessage) (*Item, string) {
	o := d.object(path, raw, roleItem)
	var it Item
	if o.kind(&it.Type) {
		o.value("options", &it.Options)
		it.Fields = o.fields()
	}
	return &it, o.problem
}

// object reads the attributes of one JSON object; once problem is set, its methods do nothing.
type object struct {
	d       *decoder
	path    string
	attrs   map[string]json.RawMessage
	problem string
}

func (d *decoder) object(path string, raw json.RawMessage, r role) *object {
	o := &object{d: d, path: path}
	if err := json.Unmarshal(raw, &o.attrs); err != nil {
		o.problem = notValid(path, err.Error())
		return o
	}
	if !d.strict {
		return o
	}
	for _, name := range slices.Sorted(maps.Keys(o.attrs)) {
		if !slices.Contains(attributes[r], name) {
			o.problem = notValid(path, fmt.Sprintf("unknown attribute %q", name))
			break
		}
	}
	return o
}

// get returns the attribute, or nil when it is absent or null.
func (o *object) get(name string) json.RawMessage {
	raw := o.attrs[name]
	if raw == nil || string(raw) == "null" {
		return nil
	}
	o.d.written[join(o.path, name)] = true
	return raw
}

func (o *object) value(name string, dst any) bool {
	raw := o.get(name)
	if o.problem != "" || raw == nil {
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		o.problem = notValid(join(o.path, name), err.Error())
		return false
	}
	return true
}

func (o *object) need(name string, dst *string) {
	if !o.value(name, dst) && o.problem == "" {
		o.problem = at(o.path, name+" is missing")
	}
}

func (o *object) flag(name string, dst *bool) {
	raw, ok := o.attrs[name]
	if o.problem != "" || !ok {
		return
	}
	value, ok := map[string]bool{"true": true, "false": false}[string(raw)]
	if !ok {
		o.problem = at(o.path, name+" must be true or false")
		return
	}
	o.d.written[join(o.path, name)] = true
	*dst = value
}

// kind reads the type and reports whether it is one this package knows.
func (o *object) kind(dst *Type) bool {
	var name string
	o.need("type", &name)
	if o.problem != "" {
		return false
	}
	if slices.Contains(types, Type(name)) {
		*dst = Type(name)
		return true
	}
	*dst = String
	if o.d.strict {
		names := make([]string, len(types))
		for i, t := range types {
			names[i] = string(t)
		}
		o.problem = at(o.path, fmt.Sprintf("type %q is not one of %s", name, strings.Join(names, ", ")))
	}
	return false
}

func (o *object) fields() []Field {
	var raws []json.RawMessage
	if !o.value("fields", &raws) {
		return nil
	}
	fields := make([]Field, len(raws))
	for i, raw := range raws {
		if fields[i], o.problem = o.d.field(fieldPath(o.path, i), raw); o.problem != "" {
			return nil
		}
	}
	return fields
}

func (o *object) item() *Item {
	raw := o.get("items")
	if o.problem != "" || raw == nil {
		return nil
	}
	it, problem := o.d.item(join(o.path, "items"), raw)
	o.problem = problem
	return it
}
