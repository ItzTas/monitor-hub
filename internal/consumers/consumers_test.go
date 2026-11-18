package consumers

import (
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"notebook", true},
		{"desktop-2", true},
		{"a", false},
		{"Notebook", false},
		{"2desk", false},
		{"desk-", false},
		{"desk--top", false},
		{"a" + strings.Repeat("b", 30), true},
		{"a" + strings.Repeat("b", 31), false},
	}
	for _, tt := range tests {
		if got := ValidName(tt.name); got != tt.want {
			t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
