// Package config loads and validates the bot configuration from the
// environment (12-factor). Load fails fast on missing required variables.
package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	WebhookSecret     string
	GitLabURL         string
	GitLabToken       string
	PrometheusURL     string
	ThrottleWarnRatio float64
	ListenAddr        string
	OpsAddr           string
	LogLevel          string
}

func Load() (*Config, error) {
	cfg := &Config{
		WebhookSecret:     os.Getenv("WEBHOOK_SECRET"),
		GitLabURL:         getenv("GITLAB_URL", "https://gitlab.com"),
		GitLabToken:       os.Getenv("GITLAB_TOKEN"),
		PrometheusURL:     os.Getenv("PROMETHEUS_URL"),
		ThrottleWarnRatio: 0.25,
		ListenAddr:        getenv("LISTEN_ADDR", ":8080"),
		OpsAddr:           getenv("OPS_ADDR", ":8081"),
		LogLevel:          getenv("LOG_LEVEL", "info"),
	}

	if v := os.Getenv("THROTTLE_WARN_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r < 0 || r > 1 {
			return nil, fmt.Errorf("THROTTLE_WARN_RATIO must be a float in [0,1], got %q", v)
		}
		cfg.ThrottleWarnRatio = r
	}

	// WEBHOOK_SECRET is only required by `serve`, which validates it itself.
	for name, val := range map[string]string{
		"GITLAB_TOKEN":   cfg.GitLabToken,
		"PROMETHEUS_URL": cfg.PrometheusURL,
	} {
		if val == "" {
			return nil, fmt.Errorf("missing required environment variable %s", name)
		}
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
