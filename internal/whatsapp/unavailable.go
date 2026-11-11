package whatsapp

import (
	"context"
	"errors"

	"github.com/ItzTas/monitor-hub/internal/notify"
)

var errUnavailable = errors.New("the WhatsApp session could not be opened when monitor-hub started; restart monitor-hub once the session store works")

type Unavailable struct{}

func (Unavailable) Name() string {
	return channelName
}

func (Unavailable) Ready() bool {
	return false
}

func (Unavailable) Send(context.Context, notify.Message) error {
	return errUnavailable
}
