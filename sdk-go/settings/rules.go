package settings

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	envPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

const defaultType = "default does not fit the field type"

func Reserved(env string) bool {
	if env == "PATH" || env == "TZ" || env == "TMPDIR" {
		return true
	}
	return slices.ContainsFunc([]string{"MONITOR_HUB_", "PLUGIN_", "LD_"}, func(prefix string) bool {
		return strings.HasPrefix(env, prefix)
	})
}

// violations walks the fields depth first and reports the first problem of each broken field.
func violations(declaration *Declaration, written map[string]bool) []string {
	w := walk{written: written}
	w.fields(declaration.Fields, "", 1, false)
	return w.found
}

type walk struct {
	written map[string]bool
	found   []string
}

func (w *walk) fields(fields []Field, parent string, level int, inList bool) {
	for i := range fields {
		c := fieldCheck{written: w.written, f: &fields[i], earlier: fields[:i], path: fieldPath(parent, i), level: level, inList: inList}
		if problem := c.problem(); problem != "" {
			w.found = append(w.found, problem)
		}
		// A field past MaxDepth is reported once, without what nests under it.
		if level > MaxDepth {
			continue
		}
		if c.f.Type == Object {
			w.fields(c.f.Fields, c.path, level+1, inList)
		}
		if c.f.Type == List && c.f.Items != nil && c.f.Items.Type == Object {
			w.fields(c.f.Items.Fields, join(c.path, "items"), level+1, true)
		}
	}
}

type fieldCheck struct {
	written map[string]bool
	f       *Field
	earlier []Field
	path    string
	level   int
	inList  bool
}

type check struct {
	broken bool
	text   string
}

// firstAt formats the first broken check for path, or returns "" when none is broken.
func firstAt(path string, checks ...check) string {
	for _, c := range checks {
		if c.broken {
			return at(path, c.text)
		}
	}
	return ""
}

// problem runs the rules in their order; each one counts on the ones before it having passed.
func (c *fieldCheck) problem() string {
	for _, rule := range []func() string{
		c.name, c.label, c.typeAttributes, c.depth, c.defaultValue, c.secret, c.editable, c.topLevelOnly, c.env, c.group,
	} {
		if problem := rule(); problem != "" {
			return problem
		}
	}
	return ""
}

func (c *fieldCheck) wrote(path, attribute string) bool {
	return c.written[join(path, attribute)]
}

func (c *fieldCheck) has(attribute string) bool {
	return c.wrote(c.path, attribute)
}

func (c *fieldCheck) name() string {
	name := c.f.Name
	return firstAt(c.path,
		check{!namePattern.MatchString(name), fmt.Sprintf("name %q does not match %s", name, namePattern)},
		check{slices.ContainsFunc(c.earlier, func(g Field) bool { return g.Name == name }), fmt.Sprintf("name %q is declared more than once", name)},
	)
}

func (c *fieldCheck) label() string {
	return firstAt(c.path, check{strings.TrimSpace(c.f.Label) == "", "label is empty"})
}

func (c *fieldCheck) typeAttributes() string {
	f := c.f
	if problem := c.members(c.path, f.Type, f.Options, f.Fields); problem != "" {
		return problem
	}
	if f.Type != List || f.Items == nil {
		return firstAt(c.path,
			check{f.Type == List, "a list needs items"},
			check{c.has("items"), "items are only for list"},
		)
	}
	path := join(c.path, "items")
	if f.Items.Type == List {
		return at(path, "a list cannot hold lists")
	}
	return c.members(path, f.Items.Type, f.Items.Options, f.Items.Fields)
}

// members checks the options and fields of the field or item at path against its type t.
func (c *fieldCheck) members(path string, t Type, options []string, fields []Field) string {
	return cmp.Or(
		c.options(path, t, options),
		firstAt(path,
			check{t == Object && len(fields) == 0, "an object needs at least one field"},
			check{t != Object && c.wrote(path, "fields"), "fields are only for object"},
		),
	)
}

func (c *fieldCheck) options(path string, t Type, options []string) string {
	if t != Choice {
		return firstAt(path, check{c.wrote(path, "options"), "options are only for choice"})
	}
	if len(options) == 0 {
		return at(path, "a choice needs at least one option")
	}
	for i, option := range options {
		if problem := firstAt(path,
			check{strings.TrimSpace(option) == "", "an option is empty"},
			check{slices.Contains(options[:i], option), fmt.Sprintf("option %q is listed more than once", option)},
		); problem != "" {
			return problem
		}
	}
	return ""
}

func (c *fieldCheck) depth() string {
	return firstAt(c.path, check{c.level > MaxDepth, fmt.Sprintf("fields nest more than %d levels deep", MaxDepth)})
}

func (c *fieldCheck) defaultValue() string {
	f := c.f
	if f.Default == nil {
		return ""
	}
	if holdsObjects(f) {
		return at(c.path, "an object or a list of objects cannot have a default")
	}
	if f.Type != List {
		return c.value(f.Type, f.Options, f.Default)
	}
	var values []json.RawMessage
	if json.Unmarshal(f.Default, &values) != nil {
		return at(c.path, defaultType)
	}
	for _, value := range values {
		if problem := c.value(f.Items.Type, f.Items.Options, value); problem != "" {
			return problem
		}
	}
	return ""
}

// value checks one default value, or one item of a list default, against the type t.
func (c *fieldCheck) value(t Type, options []string, raw json.RawMessage) string {
	// raw is JSON, so these accept exactly the numbers each type allows.
	literals := map[Type]func(string) bool{
		Integer: func(s string) bool { _, err := strconv.ParseInt(s, 10, 64); return err == nil },
		Number:  func(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil },
		Boolean: func(s string) bool { return s == "true" || s == "false" },
	}
	if fits, ok := literals[t]; ok {
		return firstAt(c.path, check{!fits(string(raw)), defaultType})
	}
	var text *string
	if json.Unmarshal(raw, &text) != nil || text == nil {
		return at(c.path, defaultType)
	}
	return firstAt(c.path,
		check{t == Choice && !slices.Contains(options, *text), fmt.Sprintf("default %q is not one of the options", *text)},
		check{t == Duration && !isDuration(*text), fmt.Sprintf("default %q is not a duration such as 15m", *text)},
	)
}

func isDuration(text string) bool {
	_, err := time.ParseDuration(text)
	return err == nil
}

func (c *fieldCheck) secret() string {
	f := c.f
	if !f.Secret {
		return ""
	}
	return firstAt(c.path,
		check{f.Default != nil, "a secret field cannot have a default"},
		check{f.Type == Object || f.Type == List, "an object or a list cannot be secret"},
		check{c.inList, "a field inside a list item cannot be secret"},
	)
}

func (c *fieldCheck) editable() string {
	f := c.f
	return firstAt(c.path,
		check{!f.Editable && strings.TrimSpace(f.Reason) == "", "editable: false needs a non-empty reason"},
		check{f.Editable && c.has("reason"), "reason is only for fields with editable: false"},
		check{!f.Editable && !c.has("env"), "editable: false needs env"},
	)
}

func (c *fieldCheck) topLevelOnly() string {
	if c.level == 1 {
		return ""
	}
	for _, attribute := range []string{"env", "group", "editable"} {
		if c.has(attribute) {
			return at(c.path, attribute+" is only for top-level fields")
		}
	}
	return ""
}

func (c *fieldCheck) env() string {
	env := c.f.Env
	if !c.has("env") {
		return ""
	}
	return firstAt(c.path,
		check{!envPattern.MatchString(env), fmt.Sprintf("env %q does not match %s", env, envPattern)},
		check{Reserved(env), fmt.Sprintf("env %q is reserved by the hub", env)},
		check{slices.ContainsFunc(c.earlier, func(g Field) bool { return g.Env == env }), fmt.Sprintf("env %q is used by more than one field", env)},
		check{holdsObjects(c.f), "env is only for fields of a simple type, choice, or a list of those"},
	)
}

func (c *fieldCheck) group() string {
	return firstAt(c.path, check{c.has("group") && strings.TrimSpace(c.f.Group) == "", "group is empty"})
}

func holdsObjects(f *Field) bool {
	return f.Type == Object || f.Type == List && f.Items.Type == Object
}
