package pages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

// idSuffix is the part of an id after f-, help-, source- or error-: rules[0].process becomes
// rules-0--process. Field names have no -, so two paths never share a suffix.
func idSuffix(path string) string {
	return strings.NewReplacer("[", "-", "]", "", ".", "--").Replace(path)
}

func controlID(path string) string {
	return "f-" + idSuffix(path)
}

// declarationKey changes with anything in the declaration, so a form drawn from an older one
// is refused.
func declarationKey(view monitorconfig.View) string {
	data, err := json.Marshal(view.Declaration)
	if err != nil {
		// Only a default that is not JSON fails, and settings.Read never gives one.
		data = []byte(err.Error())
	}
	sum := sha256.Sum256(append(data, 0, map[bool]byte{true: '1', false: '0'}[view.Declared]))
	return hex.EncodeToString(sum[:])
}
