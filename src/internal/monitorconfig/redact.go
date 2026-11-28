package monitorconfig

import "strings"

const redacted = "[redacted]"

// redact replaces every occurrence of each secret of 4 bytes or more. Bytes that any occurrence
// covers are replaced together, so a secret inside or overlapping another leaves nothing behind.
func redact(text string, secrets []string) string {
	covered := make([]bool, len(text))
	for _, secret := range secrets {
		if len(secret) < 4 {
			continue
		}
		for from := 0; ; {
			i := strings.Index(text[from:], secret)
			if i < 0 {
				break
			}
			start := from + i
			for k := start; k < start+len(secret); k++ {
				covered[k] = true
			}
			from = start + 1
		}
	}
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if !covered[i] {
			out.WriteByte(text[i])
			continue
		}
		if i == 0 || !covered[i-1] {
			out.WriteString(redacted)
		}
	}
	return out.String()
}
