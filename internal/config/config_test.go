package config

import (
	"reflect"
	"testing"
)

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

func TestLoadAuthFields(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("PROMETHEUS_URL", "http://prom")
	t.Setenv("WEBHOOK_SIGNING_TOKEN", "whsec_abc")
	t.Setenv("AUTH_METHODS", "signature,secret")

	cfg, err := Load()
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
		{name: "default is png", env: "", want: "png"},
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
			t.Setenv("CHART_FORMAT", tt.env)

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() with CHART_FORMAT=%q: want error, got %+v", tt.env, cfg)
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
		{name: "default is trace", env: "", want: "trace"},
		{name: "explicit trace", env: "trace", want: "trace"},
		{name: "explicit prometheus", env: "prometheus", want: "prometheus"},
		{name: "unknown value errors", env: "bogus", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITLAB_TOKEN", "tok")
			t.Setenv("PROMETHEUS_URL", "http://prom")
			t.Setenv("POD_RESOLVER", tt.env)

			cfg, err := Load()
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
		cfg, err := Load()
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
		cfg, err := Load()
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
		if _, err := Load(); err == nil {
			t.Fatal("Load succeeded, want error on COMMANDS_ENABLED=maybe")
		}
	})
}
