package consumers

import (
	"regexp"
	"strings"
	"testing"
)

func TestNewTokensHaveThePrefixAndAreUnique(t *testing.T) {
	hexHash := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for range 100 {
		token, hash, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(token, "mhc_") || len(token) != 47 {
			t.Fatalf("token %q: want the mhc_ prefix and 47 characters", token)
		}
		if seen[token] {
			t.Fatalf("token %q was generated twice", token)
		}
		seen[token] = true
		if hash != HashToken(token) || !hexHash.MatchString(hash) {
			t.Fatalf("hash %q is not the lowercase hex sha256 of the token", hash)
		}
	}
}
