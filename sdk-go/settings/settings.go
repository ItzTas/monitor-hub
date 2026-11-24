package settings

import (
	"encoding/json"
	"errors"
)

const (
	File          = "settings.json"
	Version       = 2
	ValidateArg   = "validate-settings"
	ConfigFDEnv   = "MONITOR_HUB_CONFIG_FD"
	DocumentLimit = 64 << 10
	MaxDepth      = 4
)

type Type string

const (
	String   Type = "string"
	Integer  Type = "integer"
	Number   Type = "number"
	Boolean  Type = "boolean"
	Duration Type = "duration"
	Choice   Type = "choice"
	Object   Type = "object"
	List     Type = "list"
)

var types = []Type{String, Integer, Number, Boolean, Duration, Choice, Object, List}

type Declaration struct{ Fields []Field }

type Field struct {
	Name, Label, Help string
	Type              Type
	Required, Secret  bool
	Editable          bool
	Reason, Group     string
	Env               string
	Options           []string
	Default           json.RawMessage
	Fields            []Field
	Items             *Item
}

type Item struct {
	Type    Type
	Options []string
	Fields  []Field
}

func Check(data []byte) []string {
	d := decoder{strict: true}
	declaration, problem := d.decode(data)
	if problem != "" {
		return []string{problem}
	}
	return violations(declaration, d.written)
}

func Read(data []byte) (*Declaration, error) {
	var d decoder
	declaration, problem := d.decode(data)
	if problem != "" {
		return nil, errors.New(problem)
	}
	if found := violations(declaration, d.written); len(found) > 0 {
		return nil, errors.New(found[0])
	}
	return declaration, nil
}

func at(path, text string) string {
	return File + ": " + located(path, text)
}

func notValid(path, text string) string {
	return File + " is not valid: " + located(path, text)
}

func located(path, text string) string {
	if path == "" {
		return text
	}
	return path + ": " + text
}
