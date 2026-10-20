// Package config loads runtime settings from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Secret is a string that never prints its value.
type Secret string

func (Secret) String() string                 { return "[REDACTED]" }
func (Secret) GoString() string               { return "[REDACTED]" }
func (Secret) LogValue() slog.Value           { return slog.StringValue("[REDACTED]") }
func (s Secret) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// Reveal returns the underlying value. Call it only where the secret is sent.
func (s Secret) Reveal() string { return string(s) }

type Binance struct {
	APIKey    Secret
	APISecret Secret
	BaseURL   string
}

// Enabled reports whether credentials are present.
func (b Binance) Enabled() bool { return b.APIKey != "" && b.APISecret != "" }

type BNC struct {
	Card     Secret
	UserID   Secret
	Password Secret
	BaseURL  string
}

// Enabled reports whether credentials are present.
func (b BNC) Enabled() bool { return b.Card != "" && b.UserID != "" && b.Password != "" }

type Config struct {
	HTTPAddr     string
	DBPath       string
	APIToken     Secret
	SyncInterval time.Duration
	// SyncLookback is how far back the first sync of a source reaches.
	SyncLookback time.Duration
	LogLevel     slog.Level
	Binance      Binance
	BNC          BNC
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	c := Config{
		HTTPAddr: env("HTTP_ADDR", "127.0.0.1:8080"),
		DBPath:   env("DB_PATH", "banker_hub.db"),
		APIToken: Secret(os.Getenv("API_TOKEN")),
		Binance: Binance{
			APIKey:    Secret(os.Getenv("BINANCE_API_KEY")),
			APISecret: Secret(os.Getenv("BINANCE_API_SECRET")),
			BaseURL:   env("BINANCE_BASE_URL", "https://api.binance.com"),
		},
		BNC: BNC{
			Card:     Secret(os.Getenv("BNC_CARD")),
			UserID:   Secret(os.Getenv("BNC_USER_ID")),
			Password: Secret(os.Getenv("BNC_PASSWORD")),
			BaseURL:  env("BNC_BASE_URL", "https://personas.bncenlinea.com"),
		},
	}

	var err error
	if c.SyncInterval, err = time.ParseDuration(env("SYNC_INTERVAL", "2h")); err != nil {
		return c, fmt.Errorf("SYNC_INTERVAL: %w", err)
	}
	if c.SyncLookback, err = time.ParseDuration(env("SYNC_LOOKBACK", "2160h")); err != nil {
		return c, fmt.Errorf("SYNC_LOOKBACK: %w", err)
	}
	if err = c.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	if c.APIToken == "" {
		return c, fmt.Errorf("API_TOKEN is required")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
