package monitorconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// strconv.ParseFloat also takes hexadecimal, underscores, inf and NaN, which the SDK refuses.
var numberPattern = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

type converter struct {
	parse func(string) (any, bool)
	// what the bridge says text is not
	not string
}

var converters = map[settings.Type]converter{
	settings.Integer: {parseInteger, "is not an integer"},
	settings.Number:  {parseNumber, "is not a number"},
	settings.Boolean: {parseBoolean, "is not true or false"},
}

func parseInteger(text string) (any, bool) {
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, false
	}
	return json.Number(strconv.FormatInt(n, 10)), true
}

func parseNumber(text string) (any, bool) {
	if !numberPattern.MatchString(text) {
		return nil, false
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) {
		return nil, false
	}
	data, err := json.Marshal(f)
	if err != nil {
		return nil, false
	}
	return json.Number(data), true
}

func parseBoolean(text string) (any, bool) {
	b, ok := map[string]bool{"true": true, "false": false}[text]
	return b, ok
}

// ConvertText returns the value of text for a field of type t, or the Expected message when text
// does not convert. Types other than integer, number and boolean take the text as it is.
func ConvertText(t settings.Type, text string) (any, string) {
	c, ok := converters[t]
	if !ok {
		return text, ""
	}
	if value, ok := c.parse(text); ok {
		return value, ""
	}
	return nil, Expected(settings.Field{Type: t})
}

func Expected(field settings.Field) string {
	if field.Type == settings.Choice {
		return "use one of: " + strings.Join(field.Options, ", ")
	}
	return map[settings.Type]string{
		settings.String:   "use text",
		settings.Duration: "use text",
		settings.Integer:  "use a whole number",
		settings.Number:   "use a number",
		settings.Boolean:  "use true or false",
		settings.List:     "use a list",
		settings.Object:   "use an object",
	}[field.Type]
}

// FromEnv builds the document from the variables of the top-level fields, as the SDK's
// from_lookup does; it returns no document when any text does not convert.
func FromEnv(decl *settings.Declaration, lookup func(string) (string, bool)) (map[string]any, []Problem) {
	doc := map[string]any{}
	var problems []Problem
	for _, f := range decl.Fields {
		if f.Env == "" {
			continue
		}
		text, ok := lookup(f.Env)
		if !ok || text == "" {
			continue
		}
		value, found := variable{f}.convert(text)
		if len(found) > 0 {
			problems = append(problems, found...)
			continue
		}
		doc[f.Name] = value
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return doc, nil
}

type variable struct{ field settings.Field }

func (v variable) convert(text string) (any, []Problem) {
	f := v.field
	if f.Secret && !utf8.ValidString(text) {
		return nil, []Problem{{f.Name, f.Env + " is not valid UTF-8"}}
	}
	text = lossy(text)
	if f.Type != settings.List {
		value, problem := v.simple(f.Type, f.Name, text)
		if problem != nil {
			return nil, []Problem{*problem}
		}
		return value, nil
	}
	return v.list(f.Items.Type, text)
}

// list splits text on commas and drops empty pieces; an index counts only the pieces kept.
func (v variable) list(t settings.Type, text string) (any, []Problem) {
	values := []any{}
	var problems []Problem
	for piece := range strings.SplitSeq(text, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		value, problem := v.simple(t, fmt.Sprintf("%s[%d]", v.field.Name, len(values)+len(problems)), piece)
		if problem != nil {
			problems = append(problems, *problem)
			continue
		}
		values = append(values, value)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return values, nil
}

func (v variable) simple(t settings.Type, path, text string) (any, *Problem) {
	c, ok := converters[t]
	if !ok {
		return text, nil
	}
	if value, ok := c.parse(text); ok {
		return value, nil
	}
	// The text of a secret stays out of the message.
	if v.field.Secret {
		return nil, &Problem{path, v.field.Env + " " + c.not}
	}
	return nil, &Problem{path, fmt.Sprintf("%s %s (%s)", v.field.Env, c.not, quote(text))}
}

// quote writes text as serde_json does: Go's encoder also escapes U+2028 and U+2029, so its own
// escapes of those two turn back into the characters.
func quote(text string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(text) // a string always encodes
	quoted := strings.TrimSuffix(b.String(), "\n")
	var out strings.Builder
	backslashes := 0
	for i := 0; i < len(quoted); i++ {
		// After an odd number of backslashes, this one is an escaped backslash of the text.
		if quoted[i] == '\\' && backslashes%2 == 0 {
			if r, ok := separators[quoted[i+1:min(i+6, len(quoted))]]; ok {
				out.WriteRune(r)
				i += 5
				backslashes = 0
				continue
			}
		}
		out.WriteByte(quoted[i])
		backslashes++
		if quoted[i] != '\\' {
			backslashes = 0
		}
	}
	return out.String()
}

var separators = map[string]rune{"u2028": '\u2028', "u2029": '\u2029'}

// lossy replaces each maximal invalid subpart with one U+FFFD, as Rust's String::from_utf8_lossy.
func lossy(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r != utf8.RuneError || size > 1 {
			out.WriteString(text[i : i+size])
			i += size
			continue
		}
		out.WriteRune(utf8.RuneError)
		i += invalidLength(text[i:])
	}
	return out.String()
}

// invalidLength is the length of the maximal subpart at the start of text, which does not begin
// a valid sequence: its lead byte and the continuation bytes that could still follow it.
func invalidLength(text string) int {
	n, lo, hi := lead(text[0])
	if n == 0 || len(text) < 2 || text[1] < lo || text[1] > hi {
		return 1
	}
	k := 2
	for k < n && k < len(text) && text[k] >= 0x80 && text[k] <= 0xBF {
		k++
	}
	return k
}

// lead returns the length of the sequence that c starts and the range of its second byte; n is 0
// for C0, C1, F5 to FF and continuation bytes, which never start one.
func lead(c byte) (n int, lo, hi byte) {
	switch {
	case c >= 0xC2 && c <= 0xDF:
		return 2, 0x80, 0xBF
	case c == 0xE0:
		return 3, 0xA0, 0xBF
	case c == 0xED:
		return 3, 0x80, 0x9F
	case c >= 0xE1 && c <= 0xEF:
		return 3, 0x80, 0xBF
	case c == 0xF0:
		return 4, 0x90, 0xBF
	case c == 0xF4:
		return 4, 0x80, 0x8F
	case c >= 0xF1 && c <= 0xF3:
		return 4, 0x80, 0xBF
	}
	return 0, 0, 0
}
