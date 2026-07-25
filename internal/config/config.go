// Package config loads and validates the bot configuration from a YAML file,
// environment variables and CLI flags (precedence: flag > env > file > default).
// Secrets are read from the environment only. Load fails fast on missing
// required values.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
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

// New returns an env-only viper: defaults set and each key bound to its env var.
// This is the base used by Load; Resolve layers a config file and CLI flags on top.
func New() *viper.Viper {
	v := viper.New()
	for _, s := range settings {
		v.SetDefault(s.key, s.def)
		_ = v.BindEnv(s.key, envName(s.key))
	}
	return v
}

// BindFlags registers a persistent flag for each non-secret setting on cmd
// (intended for the root command so subcommands inherit them), plus --config.
func BindFlags(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.String("config", "", "config file path (default search: ./config.yaml then /etc/cigar/config.yaml; also $CIGAR_CONFIG)")
	for _, s := range settings {
		f.String(flagName(s.key), s.def, s.usage)
	}
}

// Resolve builds the viper used to load configuration: env bindings (via New),
// then the persistent flags bound by BindFlags, then the config file. Precedence
// is viper's: changed flag > env > file > default. A --config / $CIGAR_CONFIG
// path that cannot be read is a hard error; the default search path is optional.
func Resolve(cmd *cobra.Command) (*viper.Viper, error) {
	v := New()
	root := cmd.Root()
	for _, s := range settings {
		if fl := root.PersistentFlags().Lookup(flagName(s.key)); fl != nil {
			_ = v.BindPFlag(s.key, fl)
		}
	}
	if err := readConfigFile(v, root); err != nil {
		return nil, err
	}
	return v, nil
}

func readConfigFile(v *viper.Viper, root *cobra.Command) error {
	path, _ := root.PersistentFlags().GetString("config")
	if path == "" {
		path = os.Getenv("CIGAR_CONFIG")
	}
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return fmt.Errorf("read config %s: %w", path, err)
		}
		return nil
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/cigar")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if errors.As(err, &notFound) {
			return nil // optional: env + defaults
		}
		return fmt.Errorf("read config: %w", err)
	}
	return nil
}

// Load extracts and validates a Config from v. Non-secret values come from v
// (flag > env > file > default); the four secrets are read from the environment
// only and never appear in v or the config file.
func Load(v *viper.Viper) (*Config, error) {
	cfg := &Config{
		WebhookSecret:       os.Getenv("WEBHOOK_SECRET"),
		WebhookSigningToken: os.Getenv("WEBHOOK_SIGNING_TOKEN"),
		GitLabToken:         os.Getenv("GITLAB_TOKEN"),
		CommandsSigningKey:  os.Getenv("COMMANDS_SIGNING_KEY"),

		GitLabURL:     v.GetString("gitlab.url"),
		PrometheusURL: v.GetString("prometheus.url"),
		PodResolver:   v.GetString("pod_resolver"),
		ListenAddr:    v.GetString("server.listen_addr"),
		OpsAddr:       v.GetString("server.ops_addr"),
		ChartFormat:   strings.ToLower(v.GetString("commands.chart_format")),
	}

	var err error
	if cfg.AuthMethods, err = authMethods(v); err != nil {
		return nil, err
	}
	if cfg.ThrottleWarnRatio, err = parseRatio(v.GetString("report.throttle_warn_ratio"), "REPORT_THROTTLE_WARN_RATIO", true); err != nil {
		return nil, err
	}
	if cfg.MemoryPressureRatio, err = parseRatio(v.GetString("report.memory_pressure_ratio"), "REPORT_MEMORY_PRESSURE_RATIO", false); err != nil {
		return nil, err
	}
	if cfg.ScrapeInterval, err = parseDuration(v.GetString("prometheus.scrape_interval"), "PROMETHEUS_SCRAPE_INTERVAL"); err != nil {
		return nil, err
	}
	if cfg.LongJobDuration, err = parseDuration(v.GetString("report.long_job_duration"), "REPORT_LONG_JOB_DURATION"); err != nil {
		return nil, err
	}
	if cfg.CommandsEnabled, err = parseBool(v.GetString("commands.enabled"), "COMMANDS_ENABLED"); err != nil {
		return nil, err
	}

	if !validPodResolvers[cfg.PodResolver] {
		return nil, fmt.Errorf("POD_RESOLVER must be one of prometheus, trace, got %q", cfg.PodResolver)
	}
	if !validChartFormats[cfg.ChartFormat] {
		return nil, fmt.Errorf("COMMANDS_CHART_FORMAT must be one of png, svg, markdown, got %q", cfg.ChartFormat)
	}
	for name, val := range map[string]string{
		"GITLAB_TOKEN":   cfg.GitLabToken,
		"PROMETHEUS_URL": cfg.PrometheusURL,
	} {
		if val == "" {
			return nil, fmt.Errorf("missing required configuration %s", name)
		}
	}
	return cfg, nil
}

// authMethods reads webhook.auth_methods, accepting either a yaml list (from the
// file) or a comma-separated string (from env/default).
func authMethods(v *viper.Viper) ([]string, error) {
	if s, ok := v.Get("webhook.auth_methods").(string); ok {
		return parseAuthMethods(s)
	}
	return parseAuthMethods(strings.Join(v.GetStringSlice("webhook.auth_methods"), ","))
}

// parseRatio parses a float in [0,1] when allowZero, else (0,1].
func parseRatio(raw, label string, allowZero bool) (float64, error) {
	r, err := strconv.ParseFloat(raw, 64)
	invalid := err != nil || r < 0 || r > 1
	if !allowZero {
		invalid = invalid || r <= 0
	}
	if invalid {
		rng := "[0,1]"
		if !allowZero {
			rng = "(0,1]"
		}
		return 0, fmt.Errorf("%s must be a float in %s, got %q", label, rng, raw)
	}
	return r, nil
}

func parseDuration(raw, label string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", label, raw)
	}
	return d, nil
}

func parseBool(raw, label string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", label, raw)
	}
	return b, nil
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
