package accounts

import (
	"regexp"
	"strings"
	"testing"
)

func TestPasswordsAreStoredAsArgon2id(t *testing.T) {
	const password = "correct horse battery"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(first, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("the hash %q does not start with the argon2id parameters", first)
	}
	if strings.Contains(first, password) {
		t.Error("the hash holds the password")
	}
	if first == second {
		t.Error("two hashes of the same password are equal")
	}
	ok, err := VerifyPassword(first, password)
	if err != nil || !ok {
		t.Errorf("the right password: %v, %v; want true", ok, err)
	}
	ok, err = VerifyPassword(first, "correct horse batterz")
	if err != nil || ok {
		t.Errorf("a wrong password: %v, %v; want false", ok, err)
	}
	for _, malformed := range []string{"", "plain", "$argon2id$v=19$m=65536,t=3,p=4$!!$!!", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$a2V5", "$argon2id$v=18$m=65536,t=3,p=4$c2FsdA$a2V5", "$argon2id$v=19$m=65536,t=0,p=4$c2FsdA$a2V5"} {
		if _, err := VerifyPassword(malformed, password); err == nil {
			t.Errorf("VerifyPassword(%q) gave no error", malformed)
		}
	}
}

func TestSecretsHaveThePrefix(t *testing.T) {
	hex := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, prefix := range []string{"mht_", "mhk_"} {
		secret, hash, err := NewSecret(prefix)
		if err != nil {
			t.Fatal(err)
		}
		if len(secret) != 47 || !regexp.MustCompile(`^`+prefix+`[A-Za-z0-9_-]{43}$`).MatchString(secret) {
			t.Errorf("the %s secret has length %d or the wrong form", prefix, len(secret))
		}
		if hash != HashSecret(secret) || !hex.MatchString(hash) {
			t.Errorf("the %s hash %q is not the hex SHA-256 of the secret", prefix, hash)
		}
	}
}
