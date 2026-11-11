package core

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

type Config struct {
	DatabaseURL string
	HTTPAddress string
	PublicURL   string
	APIToken    string
	FeedToken   string
	PluginsDir  string
	DataDir     string
	LogLevel    slog.Level
	Channels    Channels
}

func Load(environ []string, readFile func(string) ([]byte, error)) (Config, error) {
	env := make(map[string]string, len(environ))
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	r := &reader{env: env, readFile: readFile}
	secrets := map[string]Secret{}
	for _, name := range []string{"DATABASE_URL", "API_TOKEN", "FEED_TOKEN"} {
		secrets[name] = r.secret(name)
	}
	channels := r.channels()
	if err := r.err(); err != nil {
		return Config{}, err
	}
	required := map[string]string{
		"DATABASE_URL": "set the Postgres address",
		"API_TOKEN":    "set the bearer token the API requires",
	}
	for key, hint := range required {
		if secrets[key].IsZero() {
			return Config{}, fmt.Errorf("%s is empty: %s (or set %s_FILE)", key, hint, key)
		}
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(envOr(env, "LOG_LEVEL", "info"))); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error: %q", env["LOG_LEVEL"])
	}
	cfg := Config{
		DatabaseURL: secrets["DATABASE_URL"].Reveal(),
		HTTPAddress: envOr(env, "HTTP_ADDRESS", ":8080"),
		PublicURL:   strings.TrimRight(env["PUBLIC_URL"], "/"),
		APIToken:    secrets["API_TOKEN"].Reveal(),
		FeedToken:   secrets["FEED_TOKEN"].Reveal(),
		PluginsDir:  filepath.Clean(envOr(env, "PLUGINS_DIR", "/opt/monitor-hub/plugins")),
		DataDir:     filepath.Clean(envOr(env, "DATA_DIR", "/data")),
		LogLevel:    level,
		Channels:    channels,
	}
	for key, dir := range map[string]string{"PLUGINS_DIR": cfg.PluginsDir, "DATA_DIR": cfg.DataDir} {
		if !filepath.IsAbs(dir) {
			return Config{}, fmt.Errorf("%s must be an absolute path: %q", key, dir)
		}
	}
	return cfg, nil
}

func envOr(env map[string]string, key, fallback string) string {
	if value := env[key]; value != "" {
		return value
	}
	return fallback
}
