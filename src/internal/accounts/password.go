package accounts

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonSalt    = 16
	argonKey     = 32

	maxMemory = 1 << 20 // KiB
	maxTime   = 16
)

var errMalformedHash = errors.New("accounts: malformed password hash")

func checkPassword(password string) error {
	if utf8.RuneCountInString(password) < 12 || len(password) > 256 {
		return ErrWeakPassword
	}
	return nil
}

// HashPassword returns the PHC string of an argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSalt)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKey)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reads m, t and p from encoded, so hashes made with other costs keep working.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, errMalformedHash
	}
	memory, time, threads, ok := parseCosts(parts[3])
	if !ok {
		return false, errMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argonSalt {
		return false, errMalformedHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != argonKey {
		return false, errMalformedHash
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, argonKey)
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// parseCosts refuses costs above maxMemory and maxTime, so a stored hash cannot exhaust
// memory or hang a login. The round trip refuses any text after p.
func parseCosts(field string) (memory, time uint32, threads uint8, ok bool) {
	if _, err := fmt.Sscanf(field, "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return 0, 0, 0, false
	}
	ok = field == fmt.Sprintf("m=%d,t=%d,p=%d", memory, time, threads) &&
		memory <= maxMemory && time >= 1 && time <= maxTime && threads >= 1
	return memory, time, threads, ok
}
