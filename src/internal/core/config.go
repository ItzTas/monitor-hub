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
	MonitorsDir string
	DataDir     string
	LogLevel    slog.Level
	Channels    Channels
}

const databaseHint = "set the Postgres address"

func Load(environ []string, readFile func(string) ([]byte, error)) (Config, error) {
	r := newReader(environ, readFile)
	env := r.env
	databaseURL := r.secret("DATABASE_URL")
	channels := r.channels()
	if err := r.err(); err != nil {
		return Config{}, err
	}
	if databaseURL.IsZero() {
		return Config{}, missing("DATABASE_URL", databaseHint)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(envOr(env, "LOG_LEVEL", "info"))); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error: %q", env["LOG_LEVEL"])
	}
	cfg := Config{
		DatabaseURL: databaseURL.Reveal(),
		HTTPAddress: envOr(env, "HTTP_ADDRESS", ":8080"),
		PublicURL:   strings.TrimRight(env["PUBLIC_URL"], "/"),
		MonitorsDir: filepath.Clean(envOr(env, "MONITORS_DIR", "/opt/monitor-hub/monitors")),
		DataDir:     filepath.Clean(envOr(env, "DATA_DIR", "/data")),
		LogLevel:    level,
		Channels:    channels,
	}
	for key, dir := range map[string]string{"MONITORS_DIR": cfg.MonitorsDir, "DATA_DIR": cfg.DataDir} {
		if !filepath.IsAbs(dir) {
			return Config{}, fmt.Errorf("%s must be an absolute path: %q", key, dir)
		}
	}
	return cfg, nil
}

func LoadDatabaseURL(environ []string, readFile func(string) ([]byte, error)) (string, error) {
	r := newReader(environ, readFile)
	databaseURL := r.secret("DATABASE_URL")
	if err := r.err(); err != nil {
		return "", err
	}
	if databaseURL.IsZero() {
		return "", missing("DATABASE_URL", databaseHint)
	}
	return databaseURL.Reveal(), nil
}

func newReader(environ []string, readFile func(string) ([]byte, error)) *reader {
	env := make(map[string]string, len(environ))
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	return &reader{env: env, readFile: readFile}
}

func missing(key, hint string) error {
	return fmt.Errorf("%s is empty: %s (or set %s_FILE)", key, hint, key)
}

func envOr(env map[string]string, key, fallback string) string {
	if value := env[key]; value != "" {
		return value
	}
	return fallback
}
