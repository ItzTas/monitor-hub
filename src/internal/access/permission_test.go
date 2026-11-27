package access

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseAndStringRoundTrip(t *testing.T) {
	for _, slug := range []string{"monitor:sei:view", "monitor:sei:configure", "monitor:*:check", "monitor:anhanguera:hook"} {
		p, err := Parse(slug)
		if err != nil {
			t.Fatalf("Parse(%q): %v", slug, err)
		}
		if got := p.String(); got != slug {
			t.Errorf("Parse(%q).String() = %q", slug, got)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for _, slug := range []string{
		"monitor:sei:read",
		"monitor:Sei:view",
		"monitor:sei-:view",
		"monitor:sei--x:view",
		"monitor:sei",
		"monitor::view",
		"monitor:sei:view:extra",
		"events:sei:view",
		"x:monitor:sei:view",
		"",
	} {
		_, err := Parse(slug)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %v, want ErrInvalid", slug, err)
			continue
		}
		if want := fmt.Sprintf("not a permission: %q", slug); err.Error() != want {
			t.Errorf("Parse(%q) error = %q, want %q", slug, err, want)
		}
	}
}
