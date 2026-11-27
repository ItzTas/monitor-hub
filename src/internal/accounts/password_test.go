package accounts

import (
	"encoding/base64"
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
		t.Error("the hash does not start with the argon2id parameters")
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
}

func TestMalformedHashesAreRefused(t *testing.T) {
	const password = "correct horse battery"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(hash, "$")
	salt, key := parts[4], parts[5]
	rawSalt, _ := base64.RawStdEncoding.DecodeString(salt)
	rawKey, _ := base64.RawStdEncoding.DecodeString(key)
	shortSalt := base64.RawStdEncoding.EncodeToString(rawSalt[:15])
	shortKey := base64.RawStdEncoding.EncodeToString(rawKey[:31])
	build := func(variant, version, costs, salt, key string) string {
		return "$" + variant + "$" + version + "$" + costs + "$" + salt + "$" + key
	}
	const costs = "m=65536,t=3,p=4"
	if ok, err := VerifyPassword(build("argon2id", "v=19", costs, salt, key), password); !ok || err != nil {
		t.Fatalf("the rebuilt hash: %v, %v; want true", ok, err)
	}

	malformed := map[string]string{
		"not six fields":     "plain",
		"wrong variant":      build("argon2i", "v=19", costs, salt, key),
		"wrong version":      build("argon2id", "v=18", costs, salt, key),
		"t=0":                build("argon2id", "v=19", "m=65536,t=0,p=4", salt, key),
		"p=0":                build("argon2id", "v=19", "m=65536,t=3,p=0", salt, key),
		"p over 255":         build("argon2id", "v=19", "m=65536,t=3,p=256", salt, key),
		"m over the ceiling": build("argon2id", "v=19", "m=1048577,t=3,p=4", salt, key),
		"t over the ceiling": build("argon2id", "v=19", "m=65536,t=17,p=4", salt, key),
		"trailing text":      build("argon2id", "v=19", costs+",x=1", salt, key),
		"short salt":         build("argon2id", "v=19", costs, shortSalt, key),
		"short key":          build("argon2id", "v=19", costs, salt, shortKey),
		"salt not base64":    build("argon2id", "v=19", costs, "!!", key),
	}
	for name, encoded := range malformed {
		if _, err := VerifyPassword(encoded, password); err == nil {
			t.Errorf("a hash with %s gave no error", name)
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
			t.Errorf("the %s hash is not the hex SHA-256 of the secret", prefix)
		}
	}
}
