// Package config loads and validates the bot configuration from the
// environment (12-factor). Load fails fast on missing required variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// setting is one non-secret configuration knob. env and flag names are derived
// from key so the three forms (yaml/env/flag) can never drift.
type setting struct {
	key   string // viper key, e.g. "prometheus.scrape_interval"
	def   string // default value (string; viper/Load coerce as needed)
	usage string // CLI flag help text
}

// settings is the canonical list of non-secret knobs. Secrets are env-only and
// are NOT listed here.
var settings = []setting{
	{"gitlab.url", "https://gitlab.com", "GitLab instance base URL"},
	{"prometheus.url", "", "Prometheus base URL (cadvisor + kube-state-metrics)"},
	{"prometheus.scrape_interval", "30s", "Prometheus scrape interval; query windows are padded by one interval"},
	{"pod_resolver", "trace", "Pod-correlation strategy: trace or prometheus"},
	{"webhook.auth_methods", "secret", "Ordered webhook auth methods: secret,signature"},
	{"report.throttle_warn_ratio", "0.25", "Throttled-periods ratio above which a job gets a warning"},
	{"report.long_job_duration", "10m", "Job duration above which advice suggests splitting the job"},
	{"report.memory_pressure_ratio", "0.9", "Peak-memory-to-limit ratio above which OOMKill risk is warned"},
	{"commands.enabled", "false", "Enable interactive report commands"},
	{"commands.chart_format", "png", "Chart format for command replies: png, svg or markdown"},
	{"server.listen_addr", ":8080", "Webhook listen address"},
	{"server.ops_addr", ":8081", "Health/metrics (ops) listen address"},
	{"log.level", "info", "Log verbosity: debug, info, warn or error"},
}

// envName maps a viper key to its env var: uppercase, dots -> underscores.
func envName(key string) string {
	return strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// flagName maps a viper key to its CLI flag: kebab-case, dots and underscores -> dashes.
func flagName(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, ".", "-"), "_", "-")
}

type Config struct {
	WebhookSecret       string
	WebhookSigningToken string
	AuthMethods         []string
	GitLabURL           string
	GitLabToken         string
	PrometheusURL       string
	ThrottleWarnRatio   float64
	LongJobDuration     time.Duration
	MemoryPressureRatio float64
	ScrapeInterval      time.Duration
	ListenAddr          string
	OpsAddr             string
	PodResolver         string
	CommandsEnabled     bool
	CommandsSigningKey  string
	ChartFormat         string
}

func Load() (*Config, error) {
	cfg := &Config{
		WebhookSecret:       os.Getenv("WEBHOOK_SECRET"),
		WebhookSigningToken: os.Getenv("WEBHOOK_SIGNING_TOKEN"),
		GitLabURL:           getenv("GITLAB_URL", "https://gitlab.com"),
		GitLabToken:         os.Getenv("GITLAB_TOKEN"),
		PrometheusURL:       os.Getenv("PROMETHEUS_URL"),
		ThrottleWarnRatio:   0.25,
		LongJobDuration:     10 * time.Minute,
		MemoryPressureRatio: 0.9,
		ScrapeInterval:      30 * time.Second,
		ListenAddr:          getenv("LISTEN_ADDR", ":8080"),
		OpsAddr:             getenv("OPS_ADDR", ":8081"),
		PodResolver:         getenv("POD_RESOLVER", "trace"),
		CommandsSigningKey:  os.Getenv("COMMANDS_SIGNING_KEY"),
		ChartFormat:         strings.ToLower(getenv("CHART_FORMAT", "png")),
	}
	// LOG_LEVEL is consumed by the --log-level root flag (cmd/bot), not here.

	if v := os.Getenv("THROTTLE_WARN_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r < 0 || r > 1 {
			return nil, fmt.Errorf("THROTTLE_WARN_RATIO must be a float in [0,1], got %q", v)
		}
		cfg.ThrottleWarnRatio = r
	}

	if v := os.Getenv("SCRAPE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("SCRAPE_INTERVAL must be a positive duration, got %q", v)
		}
		cfg.ScrapeInterval = d
	}

	if v := os.Getenv("LONG_JOB_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("LONG_JOB_DURATION must be a positive duration, got %q", v)
		}
		cfg.LongJobDuration = d
	}

	if v := os.Getenv("MEMORY_PRESSURE_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r <= 0 || r > 1 {
			return nil, fmt.Errorf("MEMORY_PRESSURE_RATIO must be a float in (0,1], got %q", v)
		}
		cfg.MemoryPressureRatio = r
	}

	if v := os.Getenv("COMMANDS_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("COMMANDS_ENABLED must be a boolean, got %q", v)
		}
		cfg.CommandsEnabled = b
	}

	if !validPodResolvers[cfg.PodResolver] {
		return nil, fmt.Errorf("POD_RESOLVER must be one of prometheus, trace, got %q", cfg.PodResolver)
	}

	if !validChartFormats[cfg.ChartFormat] {
		return nil, fmt.Errorf("CHART_FORMAT must be one of png, svg, markdown, got %q", cfg.ChartFormat)
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

	methods, err := parseAuthMethods(os.Getenv("AUTH_METHODS"))
	if err != nil {
		return nil, err
	}
	cfg.AuthMethods = methods

	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var validAuthMethods = map[string]bool{"secret": true, "signature": true}
var validPodResolvers = map[string]bool{"prometheus": true, "trace": true}
var validChartFormats = map[string]bool{"png": true, "svg": true, "markdown": true, "md": true}

// parseAuthMethods parses the comma-separated, ordered AUTH_METHODS list.
// Order is significant (the handler tries methods in this order). An empty
// value defaults to the legacy ["secret"] for backward compatibility.
func parseAuthMethods(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{"secret"}, nil
	}
	var methods []string
	for _, part := range strings.Split(raw, ",") {
		m := strings.ToLower(strings.TrimSpace(part))
		if m == "" {
			continue
		}
		if !validAuthMethods[m] {
			return nil, fmt.Errorf("AUTH_METHODS: unknown method %q (allowed: secret, signature)", m)
		}
		methods = append(methods, m)
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("AUTH_METHODS is set but lists no valid methods")
	}
	return methods, nil
}
