package config

import (
	"testing"
	"time"
)

// loadEnv builds an env-only viper (no file, no flags) and loads it.
func loadEnv(t *testing.T) (*Config, error) {
	t.Helper()
	return Load(New())
}

func TestLoadAuthMethod(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "unset defaults to secret", env: "", want: "secret"},
		{name: "whitespace defaults to secret", env: "   ", want: "secret"},
		{name: "explicit secret", env: "secret", want: "secret"},
		{name: "explicit signing_token", env: "signing_token", want: "signing_token"},
		{name: "trims and lowercases", env: " Signing_Token ", want: "signing_token"},
		{name: "unknown method errors", env: "bogus", wantErr: true},
		{name: "list is no longer accepted", env: "secret,signing_token", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITLAB_TOKEN", "tok")
			t.Setenv("PROMETHEUS_URL", "http://prom")
			t.Setenv("WEBHOOK_AUTH_METHOD", tt.env)

			cfg, err := loadEnv(t)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("WEBHOOK_AUTH_METHOD=%q: want error, got %q", tt.env, cfg.AuthMethod)
				}
				return
			}
			if err != nil {
				t.Fatalf("WEBHOOK_AUTH_METHOD=%q: unexpected error %v", tt.env, err)
			}
			if cfg.AuthMethod != tt.want {
				t.Fatalf("AuthMethod = %q, want %q", cfg.AuthMethod, tt.want)
			}
		})
	}
}

func TestNameDerivation(t *testing.T) {
	cases := []struct{ key, env, flag string }{
		{"gitlab.url", "GITLAB_URL", "gitlab-url"},
		{"prometheus.scrape_interval", "PROMETHEUS_SCRAPE_INTERVAL", "prometheus-scrape-interval"},
		{"pod_resolver", "POD_RESOLVER", "pod-resolver"},
		{"webhook.auth_method", "WEBHOOK_AUTH_METHOD", "webhook-auth-method"},
		{"report.throttle_warn_ratio", "REPORT_THROTTLE_WARN_RATIO", "report-throttle-warn-ratio"},
		{"log.level", "LOG_LEVEL", "log-level"},
	}
	for _, c := range cases {
		if got := envName(c.key); got != c.env {
			t.Errorf("envName(%q) = %q, want %q", c.key, got, c.env)
		}
		if got := flagName(c.key); got != c.flag {
			t.Errorf("flagName(%q) = %q, want %q", c.key, got, c.flag)
		}
	}
}

func TestSettingsCoverAllKeys(t *testing.T) {
	want := []string{
		"gitlab.url", "prometheus.url", "prometheus.scrape_interval", "pod_resolver",
		"webhook.auth_method", "report.throttle_warn_ratio", "report.long_job_duration",
		"report.memory_pressure_ratio", "commands.enabled", "commands.chart_format",
		"server.listen_addr", "server.ops_addr", "log.level",
	}
	got := map[string]bool{}
	for _, s := range settings {
		got[s.key] = true
	}
	if len(got) != len(want) {
		t.Fatalf("settings has %d keys, want %d", len(got), len(want))
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("settings missing key %q", k)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("PROMETHEUS_URL", "http://prom")
	cfg, err := loadEnv(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitLabURL != "https://gitlab.com" {
		t.Errorf("GitLabURL = %q, want default", cfg.GitLabURL)
	}
	if cfg.ScrapeInterval != 30*time.Second {
		t.Errorf("ScrapeInterval = %v, want 30s", cfg.ScrapeInterval)
	}
	if cfg.ThrottleWarnRatio != 0.25 {
		t.Errorf("ThrottleWarnRatio = %v, want 0.25", cfg.ThrottleWarnRatio)
	}
	if cfg.LongJobDuration != 10*time.Minute {
		t.Errorf("LongJobDuration = %v, want 10m", cfg.LongJobDuration)
	}
	if cfg.MemoryPressureRatio != 0.9 {
		t.Errorf("MemoryPressureRatio = %v, want 0.9", cfg.MemoryPressureRatio)
	}
	if cfg.PodResolver != "trace" {
		t.Errorf("PodResolver = %q, want trace", cfg.PodResolver)
	}
	if cfg.ChartFormat != "png" {
		t.Errorf("ChartFormat = %q, want png", cfg.ChartFormat)
	}
	if cfg.ListenAddr != ":8080" || cfg.OpsAddr != ":8081" {
		t.Errorf("addrs = %q/%q, want :8080/:8081", cfg.ListenAddr, cfg.OpsAddr)
	}
	if cfg.AuthMethod != "secret" {
		t.Errorf("AuthMethod = %q, want secret", cfg.AuthMethod)
	}
}

func TestLoadRequiredFields(t *testing.T) {
	t.Run("missing GITLAB_TOKEN errors", func(t *testing.T) {
		t.Setenv("PROMETHEUS_URL", "http://prom")
		if _, err := loadEnv(t); err == nil {
			t.Fatal("Load succeeded without GITLAB_TOKEN")
		}
	})
	t.Run("missing PROMETHEUS_URL errors", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		if _, err := loadEnv(t); err == nil {
			t.Fatal("Load succeeded without PROMETHEUS_URL")
		}
	})
}

func TestLoadAuthFields(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("PROMETHEUS_URL", "http://prom")
	t.Setenv("WEBHOOK_SIGNING_TOKEN", "whsec_abc")
	t.Setenv("WEBHOOK_AUTH_METHOD", "signing_token")

	cfg, err := loadEnv(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebhookSigningToken != "whsec_abc" {
		t.Fatalf("WebhookSigningToken = %q, want %q", cfg.WebhookSigningToken, "whsec_abc")
	}
	if cfg.AuthMethod != "signing_token" {
		t.Fatalf("AuthMethod = %q, want signing_token", cfg.AuthMethod)
	}
}

func TestLoadChartFormat(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "explicit png", env: "png", want: "png"},
		{name: "explicit svg", env: "svg", want: "svg"},
		{name: "explicit markdown", env: "markdown", want: "markdown"},
		{name: "md alias", env: "md", want: "md"},
		{name: "case-insensitive", env: "SVG", want: "svg"},
		{name: "unknown value errors", env: "gif", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITLAB_TOKEN", "tok")
			t.Setenv("PROMETHEUS_URL", "http://prom")
			t.Setenv("COMMANDS_CHART_FORMAT", tt.env)
			cfg, err := loadEnv(t)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() with COMMANDS_CHART_FORMAT=%q: want error, got %+v", tt.env, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ChartFormat != tt.want {
				t.Fatalf("ChartFormat = %q, want %q", cfg.ChartFormat, tt.want)
			}
		})
	}
}

func TestLoadPodResolver(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "explicit trace", env: "trace", want: "trace"},
		{name: "explicit prometheus", env: "prometheus", want: "prometheus"},
		{name: "unknown value errors", env: "bogus", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITLAB_TOKEN", "tok")
			t.Setenv("PROMETHEUS_URL", "http://prom")
			t.Setenv("POD_RESOLVER", tt.env)
			cfg, err := loadEnv(t)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() with POD_RESOLVER=%q: want error, got %+v", tt.env, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PodResolver != tt.want {
				t.Fatalf("PodResolver = %q, want %q", cfg.PodResolver, tt.want)
			}
		})
	}
}

func TestLoadCommandsConfig(t *testing.T) {
	t.Run("defaults off with no key", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		t.Setenv("PROMETHEUS_URL", "http://prom")
		cfg, err := loadEnv(t)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.CommandsEnabled {
			t.Fatal("CommandsEnabled = true, want false by default")
		}
		if cfg.CommandsSigningKey != "" {
			t.Fatalf("CommandsSigningKey = %q, want empty", cfg.CommandsSigningKey)
		}
	})
	t.Run("reads enabled and key", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		t.Setenv("PROMETHEUS_URL", "http://prom")
		t.Setenv("COMMANDS_ENABLED", "true")
		t.Setenv("COMMANDS_SIGNING_KEY", "s3cret")
		cfg, err := loadEnv(t)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.CommandsEnabled {
			t.Fatal("CommandsEnabled = false, want true")
		}
		if cfg.CommandsSigningKey != "s3cret" {
			t.Fatalf("CommandsSigningKey = %q, want %q", cfg.CommandsSigningKey, "s3cret")
		}
	})
	t.Run("rejects non-boolean", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		t.Setenv("PROMETHEUS_URL", "http://prom")
		t.Setenv("COMMANDS_ENABLED", "maybe")
		if _, err := loadEnv(t); err == nil {
			t.Fatal("Load succeeded, want error on COMMANDS_ENABLED=maybe")
		}
	})
}

func TestLoadAdviceThresholds(t *testing.T) {
	t.Run("overrides", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		t.Setenv("PROMETHEUS_URL", "http://prom")
		t.Setenv("REPORT_LONG_JOB_DURATION", "25m")
		t.Setenv("REPORT_MEMORY_PRESSURE_RATIO", "0.75")
		cfg, err := loadEnv(t)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.LongJobDuration != 25*time.Minute {
			t.Errorf("LongJobDuration = %v, want 25m", cfg.LongJobDuration)
		}
		if cfg.MemoryPressureRatio != 0.75 {
			t.Errorf("MemoryPressureRatio = %v, want 0.75", cfg.MemoryPressureRatio)
		}
	})
	t.Run("invalid values are rejected", func(t *testing.T) {
		for _, tc := range []struct{ key, val string }{
			{"REPORT_LONG_JOB_DURATION", "soon"},
			{"REPORT_LONG_JOB_DURATION", "0s"},
			{"REPORT_LONG_JOB_DURATION", "-5m"},
			{"REPORT_MEMORY_PRESSURE_RATIO", "high"},
			{"REPORT_MEMORY_PRESSURE_RATIO", "0"},
			{"REPORT_MEMORY_PRESSURE_RATIO", "1.5"},
			{"REPORT_THROTTLE_WARN_RATIO", "nope"},
			{"REPORT_THROTTLE_WARN_RATIO", "1.5"},
			{"PROMETHEUS_SCRAPE_INTERVAL", "later"},
		} {
			t.Run(tc.key+"="+tc.val, func(t *testing.T) {
				t.Setenv("GITLAB_TOKEN", "tok")
				t.Setenv("PROMETHEUS_URL", "http://prom")
				t.Setenv(tc.key, tc.val)
				if _, err := loadEnv(t); err == nil {
					t.Fatalf("Load accepted %s=%q", tc.key, tc.val)
				}
			})
		}
	})
	t.Run("throttle zero is valid", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		t.Setenv("PROMETHEUS_URL", "http://prom")
		t.Setenv("REPORT_THROTTLE_WARN_RATIO", "0")
		cfg, err := loadEnv(t)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ThrottleWarnRatio != 0 {
			t.Errorf("ThrottleWarnRatio = %v, want 0", cfg.ThrottleWarnRatio)
		}
	})
}
