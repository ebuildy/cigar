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
