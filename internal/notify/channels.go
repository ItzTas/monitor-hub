package notify

import "github.com/ItzTas/monitor-hub/internal/core"

func Channels(cfg core.Channels, userAgent string, sessions ...Channel) []Channel {
	candidates := []struct {
		on    bool
		build func() Channel
	}{
		{cfg.Email != nil, func() Channel { return newEmail(*cfg.Email, userAgent, nil) }},
		{cfg.Telegram != nil, func() Channel { return newTelegram(*cfg.Telegram, telegramAPI, userAgent) }},
		{cfg.Ntfy != nil, func() Channel { return newNtfy(*cfg.Ntfy, userAgent) }},
		{cfg.Webhook != nil, func() Channel { return newWebhook(*cfg.Webhook, userAgent) }},
	}
	var channels []Channel
	for _, candidate := range candidates {
		if candidate.on {
			channels = append(channels, candidate.build())
		}
	}
	return append(channels, sessions...)
}

func ChannelNames(channels []Channel) []string {
	names := make([]string, 0, len(channels))
	for _, channel := range channels {
		names = append(names, channel.Name())
	}
	return names
}
