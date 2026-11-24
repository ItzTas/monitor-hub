package settings

import (
	"fmt"
	"slices"
	"strings"
)

func join(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func fieldPath(parent string, index int) string {
	return join(parent, fmt.Sprintf("fields[%d]", index))
}

// Declares reports whether path names a value of a document that follows d, such as rules[2].process.
// The empty path names the whole document.
func (d *Declaration) Declares(path string) bool {
	if path == "" {
		return true
	}
	fields := d.Fields
	for {
		end := strings.IndexAny(path, ".[")
		if end < 0 {
			end = len(path)
		}
		i := slices.IndexFunc(fields, func(f Field) bool { return f.Name == path[:end] })
		if i < 0 {
			return false
		}
		f, rest := &fields[i], path[end:]
		children := f.Fields
		if f.Type == List && strings.HasPrefix(rest, "[") {
			var ok bool
			if rest, ok = cutIndex(rest); !ok {
				return false
			}
			children = f.Items.Fields
		}
		if rest == "" {
			return true
		}
		// Only objects and items of objects have fields, so a name after anything else is not found.
		next, ok := strings.CutPrefix(rest, ".")
		if !ok {
			return false
		}
		fields, path = children, next
	}
}

// cutIndex removes a leading [index] from path, where index is 0 or has no leading zero.
func cutIndex(path string) (string, bool) {
	digits, rest, ok := strings.Cut(path[1:], "]")
	if !ok || digits == "" || len(digits) > 1 && digits[0] == '0' {
		return "", false
	}
	if strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
		return "", false
	}
	return rest, true
}
