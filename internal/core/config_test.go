package core

import (
	"log/slog"
	"reflect"
	"testing"
)

func TestLoadReadsEveryVariable(t *testing.T) {
	cfg, err := Load([]string{
		"DATABASE_URL=postgres://hub@db/hub",
		"HTTP_ADDRESS=127.0.0.1:9000",
		"PUBLIC_URL=https://hub.example.com/",
		"API_TOKEN=api-secret",
		"FEED_TOKEN=feed-secret",
		"PLUGINS_DIR=/srv/plugins/",
		"DATA_DIR=/srv/data",
		"LOG_LEVEL=debug",
		"UNRELATED=x=y",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DatabaseURL: "postgres://hub@db/hub",
		HTTPAddress: "127.0.0.1:9000",
		PublicURL:   "https://hub.example.com",
		APIToken:    "api-secret",
		FeedToken:   "feed-secret",
		PluginsDir:  "/srv/plugins",
		DataDir:     "/srv/data",
		LogLevel:    slog.LevelDebug,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v\nwant     %+v", cfg, want)
	}
}

func TestLoadFillsDefaults(t *testing.T) {
	cfg, err := Load([]string{"DATABASE_URL=postgres://db", "API_TOKEN=secret"})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DatabaseURL: "postgres://db",
		HTTPAddress: ":8080",
		APIToken:    "secret",
		PluginsDir:  "/opt/monitor-hub/plugins",
		DataDir:     "/data",
		LogLevel:    slog.LevelInfo,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v\nwant     %+v", cfg, want)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string][]string{
		"no database":          {"API_TOKEN=secret"},
		"no api token":         {"DATABASE_URL=postgres://db"},
		"relative plugins dir": {"DATABASE_URL=postgres://db", "API_TOKEN=secret", "PLUGINS_DIR=plugins"},
		"relative data dir":    {"DATABASE_URL=postgres://db", "API_TOKEN=secret", "DATA_DIR=./data"},
		"unknown log level":    {"DATABASE_URL=postgres://db", "API_TOKEN=secret", "LOG_LEVEL=loud"},
	}
	for name, environ := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(environ); err == nil {
				t.Error("Load accepted it")
			}
		})
	}
}
