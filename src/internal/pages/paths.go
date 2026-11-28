package pages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

// idSuffix is the part of an id after f-, help-, source- or error-: rules[0].process becomes
// rules-0--process. Field names have no -, so two paths never share a suffix.
func idSuffix(path string) string {
	return strings.NewReplacer("[", "-", "]", "", ".", "--").Replace(path)
}

func controlID(path string) string {
	return "f-" + idSuffix(path)
}

// variableID is the id suffix of a variable: its name, with each byte outside [A-Za-z0-9_] as
// -hex, so an odd name from the .env still makes one id. Variable names have no -.
func variableID(name string) string {
	var b strings.Builder
	for _, c := range []byte(name) {
		if c == '_' || '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "-%02x", c)
	}
	return b.String()
}

// formPaths moves each list index of the problems from the saved document to the form, list by
// list from the outside in, since the form may hold empty items the save dropped. An index
// the form does not have stays as it came.
func formPaths(problems []monitorconfig.Problem, indexes map[string][]int) []monitorconfig.Problem {
	out := make([]monitorconfig.Problem, len(problems))
	for i, p := range problems {
		out[i] = monitorconfig.Problem{Path: formPath(p.Path, indexes), Message: p.Message}
	}
	return out
}

func formPath(path string, indexes map[string][]int) string {
	var b strings.Builder
	rest := path
	for {
		open := strings.Index(rest, "[")
		end := strings.Index(rest, "]")
		if open < 0 || end < open {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:open])
		digits := rest[open+1 : end]
		form := indexes[b.String()]
		if i, err := strconv.Atoi(digits); err == nil && 0 <= i && i < len(form) {
			digits = strconv.Itoa(form[i])
		}
		b.WriteString("[" + digits + "]")
		rest = rest[end+1:]
	}
}

// declarationKey changes with anything in the declaration, so a form drawn from an older one
// is refused.
func declarationKey(view monitorconfig.View) string {
	data, err := json.Marshal(view.Declaration)
	if err != nil {
		// Only a default that is not JSON fails, and settings.Read never gives one.
		data = []byte(err.Error())
	}
	sum := sha256.Sum256(append(data, 0, map[bool]byte{true: '1', false: '0'}[view.Declared]))
	return hex.EncodeToString(sum[:])
}
