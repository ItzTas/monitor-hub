package whatsapp

import (
	"context"
	"errors"
)

var ErrNotOnWhatsApp = errors.New("the number is not on WhatsApp")

// Gateway is what carries the channel's messages to WhatsApp. A chat is an
// address the gateway understands: what Lookup returned, or a group id as
// written in the configuration.
type Gateway interface {
	Ready(ctx context.Context) bool
	Connect(ctx context.Context) error
	Lookup(ctx context.Context, number string) (string, error)
	SendText(ctx context.Context, chat, text string) error
}
