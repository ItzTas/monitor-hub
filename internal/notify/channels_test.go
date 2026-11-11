package notify

import (
	"slices"
	"testing"

	"github.com/ItzTas/monitor-hub/internal/core"
)

func TestChannelsFollowTheConfigurationWithUniqueNames(t *testing.T) {
	cfg := core.Channels{
		Email:    &core.Email{Host: "smtp.example.org", Port: 587, From: "a@example.org", To: []string{"b@example.org"}, TLS: core.TLSStartTLS},
		Telegram: &core.Telegram{Token: core.NewSecret(botToken), ChatID: "-1001234567890"},
		Ntfy:     &core.Ntfy{URL: "https://ntfy.sh/hub-topic"},
		Webhook:  &core.Webhook{URL: "https://hooks.example.org/hub", Secret: core.NewSecret(webhookSecret)},
	}
	whatsapp := &fakeChannel{name: "whatsapp"}
	cases := map[string]struct {
		cfg   core.Channels
		extra []Channel
		want  []string
	}{
		"every channel":       {cfg, []Channel{whatsapp}, []string{"email", "telegram", "ntfy", "webhook", "whatsapp"}},
		"ntfy only":           {core.Channels{Ntfy: cfg.Ntfy}, nil, []string{"ntfy"}},
		"a session's channel": {core.Channels{}, []Channel{whatsapp}, []string{"whatsapp"}},
		"none":                {core.Channels{}, nil, nil},
	}
	for name, tc := range cases {
		names := ChannelNames(Channels(tc.cfg, "monitor-hub/test", tc.extra...))
		if !slices.Equal(names, tc.want) {
			t.Errorf("%s: names = %v, want %v", name, names, tc.want)
		}
		if unique := slices.Compact(slices.Sorted(slices.Values(names))); len(unique) != len(names) {
			t.Errorf("%s: names repeat: %v", name, names)
		}
	}
}
