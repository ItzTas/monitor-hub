package pages

import (
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
)

// The templates write every attribute in double quotes, which is what these helpers read.

// text is the body as a reader sees it, with the HTML escapes undone.
func text(rec *httptest.ResponseRecorder) string {
	return html.UnescapeString(rec.Body.String())
}

// startTag is the start tag of the element with this id, or "".
func startTag(body, id string) string {
	return regexp.MustCompile(`<[a-z]+\s[^>]*\bid="` + regexp.QuoteMeta(id) + `"[^>]*>`).FindString(body)
}

// element is the name of the element a start tag opens.
func element(tag string) string {
	m := regexp.MustCompile(`^<([a-z]+)`).FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return m[1]
}

func attr(tag, name string) (string, bool) {
	m := regexp.MustCompile(`\s` + regexp.QuoteMeta(name) + `(?:="([^"]*)")?[\s/>]`).FindStringSubmatch(tag)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(m[1]), true
}

func hasAttr(tag, name, value string) bool {
	got, ok := attr(tag, name)
	return ok && got == value
}

// find is the first match of pattern in body, or "".
func find(body, pattern string) string {
	return regexp.MustCompile(pattern).FindString(body)
}

// count is how many times pattern matches body.
func count(body, pattern string) int {
	return len(regexp.MustCompile(pattern).FindAllStringIndex(body, -1))
}

// controlNames are the names of the input, select and textarea elements, in order.
func controlNames(body string) []string {
	var names []string
	for _, tag := range regexp.MustCompile(`<(?:input|select|textarea)\b[^>]*>`).FindAllString(body, -1) {
		if name, ok := attr(tag, "name"); ok {
			names = append(names, name)
		}
	}
	return names
}

// named is the start tag of the first control with this name, or "".
func named(body, name string) string {
	return regexp.MustCompile(`<(?:input|select|textarea)\s[^>]*\bname="` + regexp.QuoteMeta(html.EscapeString(name)) + `"[^>]*>`).FindString(body)
}

// box is the start tag of the checkbox with this name and value, or "".
func box(body, name, value string) string {
	for _, tag := range regexp.MustCompile(`<input\s[^>]*\btype="checkbox"[^>]*>`).FindAllString(body, -1) {
		if hasAttr(tag, "name", name) && hasAttr(tag, "value", value) {
			return tag
		}
	}
	return ""
}

// button is the start tag of the button with this value, or "".
func button(body, value string) string {
	for _, tag := range regexp.MustCompile(`<button\s[^>]*>`).FindAllString(body, -1) {
		if hasAttr(tag, "value", value) {
			return tag
		}
	}
	return ""
}

// inner is what the element with this id holds, up to its first closing tag of the same
// name, so only for elements that do not nest their own kind.
func inner(body, id string) string {
	tag := startTag(body, id)
	if tag == "" {
		return ""
	}
	rest := body[strings.Index(body, tag)+len(tag):]
	end := strings.Index(rest, "</"+element(tag)+">")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// legends are the texts of the <legend> elements, in order, as a reader sees them.
func legends(body string) []string {
	var found []string
	for _, m := range regexp.MustCompile(`(?s)<legend>(.*?)</legend>`).FindAllStringSubmatch(body, -1) {
		found = append(found, html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(m[1], "")))
	}
	return found
}

// fieldset is what the first fieldset whose legend reads legend holds, nested fieldsets
// included, or "".
func fieldset(body, legend string) string {
	open, closing := regexp.MustCompile(`<fieldset\b[^>]*>`), "</fieldset>"
	for _, at := range open.FindAllStringIndex(body, -1) {
		content := body[at[1]:]
		depth, end := 1, 0
		for depth > 0 {
			next := open.FindStringIndex(content[end:])
			stop := strings.Index(content[end:], closing)
			if stop < 0 {
				return ""
			}
			if next != nil && next[0] < stop {
				depth, end = depth+1, end+next[1]
				continue
			}
			depth, end = depth-1, end+stop+len(closing)
		}
		content = content[:end-len(closing)]
		if found := legends(content); len(found) > 0 && found[0] == legend {
			return content
		}
	}
	return ""
}

type choice struct {
	value, text string
	selected    bool
}

// options are the options of the select with this id, as a reader sees them.
func options(body, id string) []choice {
	var found []choice
	for _, m := range regexp.MustCompile(`(?s)<option\b([^>]*)>(.*?)</option>`).FindAllStringSubmatch(inner(body, id), -1) {
		value, _ := attr(m[1]+">", "value")
		_, selected := attr(m[1]+">", "selected")
		found = append(found, choice{value: value, text: html.UnescapeString(m[2]), selected: selected})
	}
	return found
}

// handlers are the start tags with an on…= attribute; a quoted value is never one.
func handlers(body string) []string {
	unquoted := regexp.MustCompile(`="[^"]*"`).ReplaceAllString(body, `=""`)
	return regexp.MustCompile(`<[^>]*\son[a-z]+=`).FindAllString(unquoted, -1)
}
