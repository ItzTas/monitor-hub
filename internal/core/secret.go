package core

import (
	"encoding/json"
	"log/slog"
)

const redacted = "[redacted]"

type Secret struct {
	value *string
}

func NewSecret(value string) Secret {
	if value == "" {
		return Secret{}
	}
	return Secret{value: &value}
}

func (s Secret) Reveal() string {
	if s.value == nil {
		return ""
	}
	return *s.value
}

func (s Secret) IsZero() bool {
	return s.value == nil
}

func (s Secret) String() string {
	return redacted
}

func (s Secret) GoString() string {
	return redacted
}

func (s Secret) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(redacted)
}

func (s Secret) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}
