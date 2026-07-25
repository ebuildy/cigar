# config.yaml + env/flag overrides — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace cigar's ~17 loose env vars with a grouped `config.yaml` loaded via viper, where every non-secret setting is overridable by an env var and a root CLI flag under one derivable naming rule (`group.key` → `GROUP_KEY` → `--group-key`), secrets stay env-only, and the Helm chart ships the file as a mounted ConfigMap.

**Architecture:** `internal/config` gains a viper-based loader driven by a single `settings` table (key + default + usage); env and flag names are pure transforms of the yaml key, so the three forms can never drift. `cmd/bot` builds one viper in the root `PersistentPreRunE` (reads file, binds env + persistent flags), resolves `log.level` to build the logger, and hands the viper to `serve`/`run` for `config.Load`. The Helm chart renders the grouped file into a ConfigMap mounted at `/etc/cigar/config.yaml` (a default search path), keeping only the four secret env vars from the Secret.

**Tech Stack:** Go 1.26, `spf13/viper` (new), `spf13/cobra`, `go.uber.org/zap`, Helm.

**Spec:** [docs/superpowers/specs/2026-07-25-config-yaml-file-design.md](../specs/2026-07-25-config-yaml-file-design.md)

---

## File Structure

- `internal/config/config.go` — **rewritten.** `Config` struct (unchanged fields), `settings` table, name-derivation helpers (`envName`/`flagName`), `New`/`BindFlags`/`Resolve`/`Load`, validation helpers. viper is the source of non-secret values; secrets via `os.Getenv`.
- `internal/config/config_test.go` — **rewritten** to the new API and renamed env vars; keeps all existing coverage plus a precedence test.
- `cmd/bot/main.go` — **modified.** Root wires `config.BindFlags`; `PersistentPreRunE` builds the viper + logger; removes the hand-rolled `--log-level` flag and `envOr`.
- `cmd/bot/serve.go`, `cmd/bot/run.go` — **modified.** `config.Load(cfgViper)` instead of `config.Load()`.
- `deploy/chart/cigar/templates/configmap.yaml` — **new.** Renders `config.yaml`.
- `deploy/chart/cigar/templates/deployment.yaml` — **modified.** Mount ConfigMap, drop non-secret env, keep secret env, add `checksum/config`.
- `deploy/chart/cigar/values.yaml` — **modified.** `config.*` restructured to the grouped shape.
- `README.md`, `docs/usage.md`, `CLAUDE.md` — **modified.** Docs + approved-deps.
- `config.yaml` (repo root) — already created.

**Note on `internal/e2e`:** it does **not** import `internal/config` (it wires `reporter.Reporter` directly) and sets none of the renamed env vars, so it needs no code change — only a confirming test run.

---

## Task 1: Add viper and the settings table + name derivation

**Files:**
- Modify: `go.mod`, `go.sum` (via `go get`)
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Add the viper dependency**

Run:
```bash
go get github.com/spf13/viper@latest && go mod tidy
```
Expected: `go.mod` now lists `github.com/spf13/viper`; no build errors.

- [ ] **Step 2: Write the failing test for name derivation**

Add to `internal/config/config_test.go` (leave the existing tests in place for now — they still call the old `Load()` and will be migrated in Task 2):

```go
func TestNameDerivation(t *testing.T) {
	cases := []struct {
		key, env, flag string
	}{
		{"gitlab.url", "GITLAB_URL", "gitlab-url"},
		{"prometheus.scrape_interval", "PROMETHEUS_SCRAPE_INTERVAL", "prometheus-scrape-interval"},
		{"pod_resolver", "POD_RESOLVER", "pod-resolver"},
		{"webhook.auth_methods", "WEBHOOK_AUTH_METHODS", "webhook-auth-methods"},
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
		"webhook.auth_methods", "report.throttle_warn_ratio", "report.long_job_duration",
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
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/config/ -run 'TestNameDerivation|TestSettingsCoverAllKeys'`
Expected: FAIL — `undefined: envName`, `undefined: flagName`, `undefined: settings`.

- [ ] **Step 4: Add the settings table and derivation helpers**

At the top of `internal/config/config.go`, after the imports, add (keep the existing `Config` struct and the existing `Load`/`getenv`/`parseAuthMethods` for now — Task 2 replaces `Load`):

```go
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
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/config/ -run 'TestNameDerivation|TestSettingsCoverAllKeys'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add viper dep, settings table and name derivation

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: viper-based New() + Load() with validation; migrate existing tests

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests (rewrite the env-driven tests to the new API + renamed env vars)**

Replace the whole body of `internal/config/config_test.go` with the following (this supersedes `TestLoadAuthFields`, `TestLoadChartFormat`, `TestLoadPodResolver`, `TestLoadCommandsConfig`, `TestLoadAdviceThresholds`; keeps `TestParseAuthMethods`, `TestNameDerivation`, `TestSettingsCoverAllKeys`):

```go
package config

import (
	"reflect"
	"testing"
	"time"
)

// loadEnv builds an env-only viper (no file, no flags) and loads it.
func loadEnv(t *testing.T) (*Config, error) {
	t.Helper()
	return Load(New())
}

func TestParseAuthMethods(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty defaults to secret", raw: "", want: []string{"secret"}},
		{name: "whitespace defaults to secret", raw: "   ", want: []string{"secret"}},
		{name: "single signature", raw: "signature", want: []string{"signature"}},
		{name: "ordered pair", raw: "secret,signature", want: []string{"secret", "signature"}},
		{name: "reversed order preserved", raw: "signature,secret", want: []string{"signature", "secret"}},
		{name: "trims and lowercases", raw: " Secret , SIGNATURE ", want: []string{"secret", "signature"}},
		{name: "skips empty entries", raw: "secret,,signature", want: []string{"secret", "signature"}},
		{name: "unknown method errors", raw: "secret,bogus", wantErr: true},
		{name: "only commas errors", raw: ",,", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAuthMethods(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseAuthMethods(%q) = %v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAuthMethods(%q): unexpected error %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseAuthMethods(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNameDerivation(t *testing.T) {
	cases := []struct{ key, env, flag string }{
		{"gitlab.url", "GITLAB_URL", "gitlab-url"},
		{"prometheus.scrape_interval", "PROMETHEUS_SCRAPE_INTERVAL", "prometheus-scrape-interval"},
		{"pod_resolver", "POD_RESOLVER", "pod-resolver"},
		{"webhook.auth_methods", "WEBHOOK_AUTH_METHODS", "webhook-auth-methods"},
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
		"webhook.auth_methods", "report.throttle_warn_ratio", "report.long_job_duration",
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
	if !reflect.DeepEqual(cfg.AuthMethods, []string{"secret"}) {
		t.Errorf("AuthMethods = %v, want [secret]", cfg.AuthMethods)
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
	t.Setenv("WEBHOOK_AUTH_METHODS", "signature,secret")

	cfg, err := loadEnv(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebhookSigningToken != "whsec_abc" {
		t.Fatalf("WebhookSigningToken = %q, want %q", cfg.WebhookSigningToken, "whsec_abc")
	}
	if got, want := cfg.AuthMethods, []string{"signature", "secret"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AuthMethods = %v, want %v", got, want)
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: New`, and `Load` still has the old no-arg signature (compile error).

- [ ] **Step 3: Replace `Load` and add `New` + validation helpers**

In `internal/config/config.go`, **delete** the current `func Load() (*Config, error)` and the now-unused `getenv` helper, and add the following. Add `"github.com/spf13/viper"` to the imports (keep `fmt`, `os`, `strconv`, `strings`, `time`; drop any that become unused — the compiler will tell you):

```go
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
```

Also update the package doc comment at the top of the file to reflect the new source:

```go
// Package config loads and validates the bot configuration from a YAML file,
// environment variables and CLI flags (precedence: flag > env > file > default).
// Secrets are read from the environment only. Load fails fast on missing
// required values.
```

- [ ] **Step 4: Run the full config package tests**

Run: `go test ./internal/config/`
Expected: PASS (all tests). If the compiler flags an unused import (`os` is still used for secrets; `time` still used), remove only genuinely unused ones.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): load non-secret values via viper, secrets env-only

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Flag binding, config-file resolution, and precedence test

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/resolve_test.go` (new)

- [ ] **Step 1: Write the failing precedence test**

Create `internal/config/resolve_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot returns a root command with config flags bound and the given args.
func newTestRoot(args ...string) *cobra.Command {
	root := &cobra.Command{Use: "bot", RunE: func(*cobra.Command, []string) error { return nil }}
	BindFlags(root)
	root.SetArgs(args)
	return root
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestResolvePrecedence(t *testing.T) {
	file := writeConfig(t, "report:\n  throttle_warn_ratio: 0.10\n")

	t.Run("file value used when no env or flag", func(t *testing.T) {
		root := newTestRoot("--config", file)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		v, err := Resolve(root)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got := v.GetString("report.throttle_warn_ratio"); got != "0.10" {
			t.Fatalf("got %q, want file value 0.10", got)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		t.Setenv("REPORT_THROTTLE_WARN_RATIO", "0.20")
		root := newTestRoot("--config", file)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		v, err := Resolve(root)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got := v.GetString("report.throttle_warn_ratio"); got != "0.20" {
			t.Fatalf("got %q, want env value 0.20", got)
		}
	})

	t.Run("flag overrides env and file", func(t *testing.T) {
		t.Setenv("REPORT_THROTTLE_WARN_RATIO", "0.20")
		root := newTestRoot("--config", file, "--report-throttle-warn-ratio", "0.30")
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		v, err := Resolve(root)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got := v.GetString("report.throttle_warn_ratio"); got != "0.30" {
			t.Fatalf("got %q, want flag value 0.30", got)
		}
	})
}

func TestResolveMissingExplicitFileErrors(t *testing.T) {
	root := newTestRoot("--config", "/no/such/config.yaml")
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil {
		t.Fatal("Resolve succeeded with a missing explicit --config path")
	}
}

func TestResolveNoFileFallsBackToEnv(t *testing.T) {
	// No --config, and t.TempDir cwd has no config.yaml -> env/defaults only.
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("REPORT_THROTTLE_WARN_RATIO", "0.42")
	root := newTestRoot()
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	v, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := v.GetString("report.throttle_warn_ratio"); got != "0.42" {
		t.Fatalf("got %q, want env value 0.42", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run 'TestResolve'`
Expected: FAIL — `undefined: BindFlags`, `undefined: Resolve`.

- [ ] **Step 3: Add `BindFlags` and `Resolve`**

Add to `internal/config/config.go` (add `"errors"` and `"github.com/spf13/cobra"` to imports):

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/`
Expected: PASS (all tests, including precedence).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/resolve_test.go
git commit -m "feat(config): bind CLI flags and resolve config file with precedence

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Wire cmd/bot to the new config

**Files:**
- Modify: `cmd/bot/main.go`
- Modify: `cmd/bot/serve.go:47`
- Modify: `cmd/bot/run.go:36`

- [ ] **Step 1: Update `main.go` to bind flags and build the viper + logger in PersistentPreRunE**

In `cmd/bot/main.go`:

1. Add imports `"github.com/spf13/viper"` and `"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"`. Remove `"golang.org/x/term"`? No — `term.IsTerminal` is still used; keep it.
2. Add a package-level var next to `logger`:

```go
// cfgViper is the resolved configuration built once in the root
// PersistentPreRunE and consumed by the serve/run subcommands.
var cfgViper *viper.Viper
```

3. Replace the root command block. Change `main()` so the root no longer declares `logLevel`/registers `--log-level` itself, and `PersistentPreRunE` builds the viper and logger:

```go
func main() {
	root := &cobra.Command{
		Use:           "bot",
		Short:         "Posts CI pipeline resource-usage reports as GitLab MR comments",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if term.IsTerminal(int(os.Stdout.Fd())) {
				_, _ = os.Stdout.WriteString(banner(true))
			}
			v, err := config.Resolve(cmd)
			if err != nil {
				return err
			}
			cfgViper = v
			log, err := newLogger(v.GetString("log.level"))
			if err != nil {
				return err
			}
			logger = log
			return nil
		},
	}
	config.BindFlags(root)
	root.AddCommand(newServeCmd(), newRunCmd(), newAdviseCmd())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		if logger != nil {
			logger.Error("fatal", zap.Error(err))
			_ = logger.Sync()
		} else {
			fmt.Fprintln(os.Stderr, "fatal:", err)
		}
		os.Exit(1)
	}
	if logger != nil {
		_ = logger.Sync()
	}
}
```

4. Delete the now-unused `envOr` function (lines 103-110) — its only caller was the removed `--log-level` default.

- [ ] **Step 2: Update `serve.go` and `run.go` to use the resolved viper**

In `cmd/bot/serve.go`, change the `serve` function's first lines:

```go
func serve(ctx context.Context) error {
	cfg, err := config.Load(cfgViper)
	if err != nil {
		return err
	}
```

In `cmd/bot/run.go`, inside the `RunE`, change:

```go
				cfg, err := config.Load(cfgViper)
				if err != nil {
					return err
				}
```

- [ ] **Step 3: Build and run the CLI smoke checks**

Run:
```bash
go build ./cmd/bot && \
GITLAB_TOKEN=x PROMETHEUS_URL=http://p ./bot run --project 1 999 --log-level error --help >/dev/null && \
echo OK-help
```
Expected: prints `OK-help` (help path works; PersistentPreRunE resolves the repo-root `config.yaml` without requiring secrets).

Then verify a flag override reaches config (using `advise`/`run` requires network, so assert via `--help` listing the new flags):
```bash
./bot --help | grep -E 'prometheus-scrape-interval|report-throttle-warn-ratio|webhook-auth-methods|config'
```
Expected: the new persistent flags are listed.

- [ ] **Step 4: Run the whole test suite (compile check for cmd/bot)**

Run: `go test ./cmd/... ./internal/config/...`
Expected: PASS. `serve_test.go` (which builds `config.Config` literals and calls `buildAuthenticators`) is unaffected and compiles.

- [ ] **Step 5: Commit**

```bash
git add cmd/bot/main.go cmd/bot/serve.go cmd/bot/run.go
git commit -m "feat(cmd): wire config.yaml + flags via viper in root command

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Helm — ConfigMap, mounted file, grouped values

**Files:**
- Create: `deploy/chart/cigar/templates/configmap.yaml`
- Modify: `deploy/chart/cigar/templates/deployment.yaml`
- Modify: `deploy/chart/cigar/values.yaml`

- [ ] **Step 1: Restructure the `config` block in `values.yaml`**

Replace the existing `config:` block (and remove the separate top-level `commands:` block, folding `enabled` under `config.commands`) with:

```yaml
# Bot configuration. Rendered into a ConfigMap as /etc/cigar/config.yaml.
# Every key maps to an env var (GROUP_KEY) and a CLI flag (--group-key); the
# file is the source, env/flags override. Secrets are NOT here — see secrets.*.
config:
  gitlab:
    url: "https://gitlab.com"
  prometheus:
    # Must scrape cadvisor and kube-state-metrics.
    url: "http://prometheus-operated.monitoring.svc:9090"
    scrapeInterval: "30s"
  # Pod-correlation strategy: "trace" or "prometheus".
  podResolver: "trace"
  webhook:
    # Ordered auth methods; empty defaults to [secret]. e.g. [secret, signature].
    authMethods: []
  report:
    throttleWarnRatio: "0.25"
    longJobDuration: "10m"
    memoryPressureRatio: "0.9"
  commands:
    # Interactive report commands. Requires secrets.commandsSigningKey when true.
    enabled: false
    chartFormat: "png"
  server:
    listenAddr: ":8080"
    opsAddr: ":8081"
  log:
    level: "info"
```

Leave the `secrets:` block unchanged.

- [ ] **Step 2: Create the ConfigMap template**

Create `deploy/chart/cigar/templates/configmap.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "cigar.fullname" . }}
  labels:
    {{- include "cigar.labels" . | nindent 4 }}
data:
  config.yaml: |
    gitlab:
      url: {{ .Values.config.gitlab.url | quote }}
    prometheus:
      url: {{ .Values.config.prometheus.url | quote }}
      scrape_interval: {{ .Values.config.prometheus.scrapeInterval | quote }}
    pod_resolver: {{ .Values.config.podResolver | quote }}
    webhook:
      auth_methods: {{ .Values.config.webhook.authMethods | default (list "secret") | toJson }}
    report:
      throttle_warn_ratio: {{ .Values.config.report.throttleWarnRatio | quote }}
      long_job_duration: {{ .Values.config.report.longJobDuration | quote }}
      memory_pressure_ratio: {{ .Values.config.report.memoryPressureRatio | quote }}
    commands:
      enabled: {{ .Values.config.commands.enabled | quote }}
      chart_format: {{ .Values.config.commands.chartFormat | quote }}
    server:
      listen_addr: {{ .Values.config.server.listenAddr | quote }}
      ops_addr: {{ .Values.config.server.opsAddr | quote }}
    log:
      level: {{ .Values.config.log.level | quote }}
```

- [ ] **Step 3: Rewrite the `env:`, `volumeMounts`, `volumes`, and annotations in `deployment.yaml`**

Make three edits in `deploy/chart/cigar/templates/deployment.yaml`:

**(a)** Replace the pod `annotations` block (currently `{{- with .Values.podAnnotations }}...`) so the config checksum is always present:

```yaml
      annotations:
        checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
        {{- with .Values.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
```

**(b)** Replace the entire `env:` block (currently lines ~52-105, from `env:` through the `extraEnv` `{{- end }}`) with the secret-only version. Auth-method gating now reads the list under `config.webhook.authMethods`:

```yaml
          env:
            {{- $methods := .Values.config.webhook.authMethods | default (list "secret") }}
            {{- if or (not .Values.config.webhook.authMethods) (has "secret" $methods) }}
            - name: WEBHOOK_SECRET
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.secrets.existingSecret | default (include "cigar.fullname" .) }}
                  key: WEBHOOK_SECRET
            {{- end }}
            {{- if has "signature" $methods }}
            - name: WEBHOOK_SIGNING_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.secrets.existingSecret | default (include "cigar.fullname" .) }}
                  key: WEBHOOK_SIGNING_TOKEN
            {{- end }}
            - name: GITLAB_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.secrets.existingSecret | default (include "cigar.fullname" .) }}
                  key: GITLAB_TOKEN
            {{- if .Values.config.commands.enabled }}
            - name: COMMANDS_SIGNING_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.secrets.existingSecret | default (include "cigar.fullname" .) }}
                  key: COMMANDS_SIGNING_KEY
            {{- end }}
            {{- with .Values.extraEnv }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
```

**(c)** Replace the `volumeMounts` and `volumes` blocks so the ConfigMap is always mounted at `/etc/cigar`:

```yaml
          volumeMounts:
            - name: config
              mountPath: /etc/cigar
              readOnly: true
            {{- with .Values.volumeMounts }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
```

and (at the pod-spec level, replacing the existing `{{- with .Values.volumes }}` block):

```yaml
      volumes:
        - name: config
          configMap:
            name: {{ include "cigar.fullname" . }}
        {{- with .Values.volumes }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
```

- [ ] **Step 4: Lint and render the chart**

Run:
```bash
helm lint deploy/chart/cigar
helm template t deploy/chart/cigar > /tmp/cigar-render.yaml
```
Expected: `helm lint` reports 0 failures; template renders without error.

- [ ] **Step 5: Assert the render is correct**

Run:
```bash
# ConfigMap contains the grouped file:
grep -q 'scrape_interval: "30s"' /tmp/cigar-render.yaml && echo CM-OK
# No non-secret env leaked into the Deployment:
! grep -E 'name: (PROMETHEUS_URL|GITLAB_URL|SCRAPE_INTERVAL|THROTTLE_WARN_RATIO|POD_RESOLVER|LOG_LEVEL|CHART_FORMAT)' /tmp/cigar-render.yaml && echo NO-NONSECRET-ENV
# Secrets still present:
grep -q 'key: GITLAB_TOKEN' /tmp/cigar-render.yaml && echo SECRET-OK
# Config mounted + checksum present:
grep -q 'mountPath: /etc/cigar' /tmp/cigar-render.yaml && grep -q 'checksum/config' /tmp/cigar-render.yaml && echo MOUNT-OK
```
Expected: prints `CM-OK`, `NO-NONSECRET-ENV`, `SECRET-OK`, `MOUNT-OK`.

Then render with signature auth + commands to check gating:
```bash
helm template t deploy/chart/cigar \
  --set 'config.webhook.authMethods={secret,signature}' \
  --set config.commands.enabled=true | \
  grep -E 'name: (WEBHOOK_SECRET|WEBHOOK_SIGNING_TOKEN|COMMANDS_SIGNING_KEY)'
```
Expected: all three secret env entries appear.

- [ ] **Step 6: Commit**

```bash
git add deploy/chart/cigar/templates/configmap.yaml deploy/chart/cigar/templates/deployment.yaml deploy/chart/cigar/values.yaml
git commit -m "feat(chart): ship config.yaml as a mounted ConfigMap; secrets stay env

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: Documentation and approved-deps

**Files:**
- Modify: `README.md`
- Modify: `docs/usage.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update the README Configuration section**

In `README.md`, replace the env-var table (the block starting at the `## Configuration` heading, ~line 70, through the table that ends before line 96's migration note) with a config-first section. Use this content:

````markdown
## Configuration

cigar reads a **`config.yaml`** file (see [`config.yaml`](config.yaml) at the repo
root for a documented sample). Every non-secret setting is overridable by an
environment variable and a CLI flag under one rule:

> **`group.key`** in the file  →  **`GROUP_KEY`** env var  →  **`--group-key`** flag
> — precedence: **flag > env > file > default**.

The file is found via `--config` / `$CIGAR_CONFIG`, else `./config.yaml`, else
`/etc/cigar/config.yaml`; if none exists, env + defaults are used (so `bot run`
works with just env).

| yaml path | env | default | notes |
|---|---|---|---|
| `gitlab.url` | `GITLAB_URL` | `https://gitlab.com` | GitLab instance base URL |
| `prometheus.url` | `PROMETHEUS_URL` | — (required) | cadvisor + kube-state-metrics scraped |
| `prometheus.scrape_interval` | `PROMETHEUS_SCRAPE_INTERVAL` | `30s` | query windows padded by one interval |
| `pod_resolver` | `POD_RESOLVER` | `trace` | `trace` or `prometheus` |
| `webhook.auth_methods` | `WEBHOOK_AUTH_METHODS` | `secret` | ordered: `secret`, `signature` |
| `report.throttle_warn_ratio` | `REPORT_THROTTLE_WARN_RATIO` | `0.25` | ⚠️ warning threshold |
| `report.long_job_duration` | `REPORT_LONG_JOB_DURATION` | `10m` | advice: split long jobs |
| `report.memory_pressure_ratio` | `REPORT_MEMORY_PRESSURE_RATIO` | `0.9` | advice: OOMKill risk |
| `commands.enabled` | `COMMANDS_ENABLED` | `false` | interactive report commands |
| `commands.chart_format` | `COMMANDS_CHART_FORMAT` | `png` | `png`, `svg` or `markdown` |
| `server.listen_addr` | `SERVER_LISTEN_ADDR` | `:8080` | webhook listen address |
| `server.ops_addr` | `SERVER_OPS_ADDR` | `:8081` | health/metrics address |
| `log.level` | `LOG_LEVEL` | `info` | `--log-level` also available |

**Secrets — environment only, never in the file:** `WEBHOOK_SECRET` (`serve`,
if `secret` enabled), `WEBHOOK_SIGNING_TOKEN` (`serve`, if `signature` enabled),
`GITLAB_TOKEN` (always), `COMMANDS_SIGNING_KEY` (`serve`, if commands enabled).
`bot run` needs neither webhook secret.
````

Keep the existing migration paragraph but update the env var name: change
`AUTH_METHODS=secret,signature` / `AUTH_METHODS=signature` to
`WEBHOOK_AUTH_METHODS=secret,signature` / `WEBHOOK_AUTH_METHODS=signature`.

- [ ] **Step 2: Update the README Helm/deploy note**

In the Helm section (~line 148), replace the sentence beginning "All bot env vars map to `config.*` values…" with:

```markdown
The chart renders `config.*` values into a ConfigMap mounted at
`/etc/cigar/config.yaml`; secrets come from an existing Secret (recommended) or
`secrets.webhookSecret`/`secrets.signingToken`/`secrets.gitlabToken`. Enable
signing-token auth via `config.webhook.authMethods` (e.g. `{secret,signature}`
during migration, then `{signature}`).
```

Also update the `--set` example at ~line 144: change `config.prometheusUrl=...`
to `config.prometheus.url=...`.

- [ ] **Step 3: Update `docs/usage.md`**

Add a "Configuration" section near the top of `docs/usage.md` (after any intro,
before deployment steps):

````markdown
## Configuration

cigar is configured by a `config.yaml` file, with env vars and CLI flags
overriding it. The three forms of every non-secret setting derive from one yaml
path: `group.key` → `GROUP_KEY` → `--group-key`. Precedence, highest first:

```
--flag  >  ENV_VAR  >  config.yaml  >  built-in default
```

**File discovery:** `--config <path>` or `$CIGAR_CONFIG`, else `./config.yaml`,
else `/etc/cigar/config.yaml`. No file found → env + defaults only.

**Secrets are environment-only** and never read from the file: `WEBHOOK_SECRET`,
`WEBHOOK_SIGNING_TOKEN`, `GITLAB_TOKEN`, `COMMANDS_SIGNING_KEY`.

See the annotated [`config.yaml`](../config.yaml) at the repo root for every key.
In Kubernetes the Helm chart renders these into a ConfigMap mounted at
`/etc/cigar/config.yaml`.
````

If `docs/usage.md` references any renamed env var (grep it for `AUTH_METHODS`,
`SCRAPE_INTERVAL`, `THROTTLE_WARN_RATIO`, `LONG_JOB_DURATION`,
`MEMORY_PRESSURE_RATIO`, `CHART_FORMAT`, `LISTEN_ADDR`, `OPS_ADDR`), update those
occurrences to the new names from the table above.

- [ ] **Step 4: Update `CLAUDE.md`**

Two edits in `CLAUDE.md`:

1. In the approved-deps sentence under "Go conventions", add viper:
   change `spf13/cobra` (CLI) to `spf13/cobra` (CLI) + `spf13/viper` (config).
2. Replace the `### Config (env only, 12-factor)` section's first paragraph so it
   describes the file + precedence model. Use:

````markdown
### Config (config.yaml + env + flags)

Configuration is a grouped `config.yaml` loaded via `spf13/viper`, with env vars
and CLI flags overriding it (precedence: **flag > env > file > default**). Every
non-secret setting derives its three forms from one yaml path:
`group.key` → `GROUP_KEY` env → `--group-key` flag (see `internal/config`'s
`settings` table — the single source of truth). File discovery: `--config` /
`$CIGAR_CONFIG`, else `./config.yaml`, else `/etc/cigar/config.yaml`; missing →
env + defaults. Secrets are **environment-only**, never in the file:
`WEBHOOK_SECRET`, `WEBHOOK_SIGNING_TOKEN`, `GITLAB_TOKEN`, `COMMANDS_SIGNING_KEY`.
`WEBHOOK_SECRET`/`WEBHOOK_SIGNING_TOKEN` are required by `serve` only, per enabled
auth method; `bot run` needs neither.
````

- [ ] **Step 5: Commit**

```bash
git add README.md docs/usage.md CLAUDE.md
git commit -m "docs: document config.yaml, env/flag mapping and renamed env vars

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Lint + full test suite with race detector**

Run: `mise r lint test`
Expected: golangci-lint clean; `go test -race ./...` all PASS, including `internal/e2e` (unchanged) and `internal/config`.

- [ ] **Step 2: Build the binary**

Run: `mise r build`
Expected: builds `./cmd/bot` with no errors.

- [ ] **Step 3: Chart lint + template**

Run: `helm lint deploy/chart/cigar && helm template t deploy/chart/cigar >/dev/null`
Expected: 0 failures, renders cleanly.

- [ ] **Step 4: Confirm the root config.yaml is consistent with defaults**

Run:
```bash
go build ./cmd/bot && GITLAB_TOKEN=x PROMETHEUS_URL=http://p ./bot run --help >/dev/null && echo CONFIG-LOADS
```
Expected: prints `CONFIG-LOADS` (root `config.yaml` parses and is picked up by the default search path).

- [ ] **Step 5: Final commit (if any stray formatting)**

```bash
gofmt -w ./internal/config ./cmd/bot
git diff --quiet || git commit -am "chore: gofmt

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review Notes

- **Spec coverage:** naming rule (Task 1), precedence + file discovery + secrets-env-only (Tasks 2–3), log-level wiring (Task 4), ConfigMap + mount + checksum + secret-only env (Task 5), values restructure (Task 5), docs + approved-deps + root config.yaml reference (Task 6), tests incl. precedence and test-isolation via `t.Chdir`/`t.TempDir` (Tasks 2–3, 7). e2e untouched per the actual import graph (verified: e2e does not import `internal/config`).
- **Renamed env vars:** the 7 renames are exercised by the migrated tests (Task 2) and asserted absent from the Deployment render (Task 5).
- **Type consistency:** `New`, `Load(v)`, `BindFlags(cmd)`, `Resolve(cmd)`, `envName`, `flagName`, `settings`, `setting`, `authMethods`, `parseRatio`, `parseDuration`, `parseBool` are defined once and used with the same signatures across tasks; `cfgViper` is the single shared viper in `cmd/bot`.
- **Validation parity:** ratios/durations/bools are parsed from viper **strings** (not `GetFloat64`/`GetDuration`/`GetBool`) so malformed values still error, matching the pre-refactor tests; `throttle=0` remains valid.
