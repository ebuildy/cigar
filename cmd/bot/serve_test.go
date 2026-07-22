package main

import (
	"testing"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
)

func TestBuildAuthenticators(t *testing.T) {
	signing := "whsec_" + "MDEyMzQ1Njc4OWFiY2RlZg==" // base64("0123456789abcdef")

	tests := []struct {
		name      string
		cfg       *config.Config
		wantNames []string
		wantErr   bool
	}{
		{
			name:      "secret only",
			cfg:       &config.Config{AuthMethods: []string{"secret"}, WebhookSecret: "x"},
			wantNames: []string{"secret"},
		},
		{
			name:      "signature only",
			cfg:       &config.Config{AuthMethods: []string{"signature"}, WebhookSigningToken: signing},
			wantNames: []string{"signature"},
		},
		{
			name:      "ordered pair preserves order",
			cfg:       &config.Config{AuthMethods: []string{"signature", "secret"}, WebhookSecret: "x", WebhookSigningToken: signing},
			wantNames: []string{"signature", "secret"},
		},
		{
			name:    "secret enabled but unset",
			cfg:     &config.Config{AuthMethods: []string{"secret"}},
			wantErr: true,
		},
		{
			name:    "signature enabled but unset",
			cfg:     &config.Config{AuthMethods: []string{"signature"}},
			wantErr: true,
		},
		{
			name:    "signature token invalid",
			cfg:     &config.Config{AuthMethods: []string{"signature"}, WebhookSigningToken: "whsec_@@@"},
			wantErr: true,
		},
		{
			name:    "empty methods yields error",
			cfg:     &config.Config{AuthMethods: nil},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auths, err := buildAuthenticators(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", auths)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var names []string
			for _, a := range auths {
				names = append(names, a.Name())
			}
			if len(names) != len(tt.wantNames) {
				t.Fatalf("names = %v, want %v", names, tt.wantNames)
			}
			for i := range names {
				if names[i] != tt.wantNames[i] {
					t.Fatalf("names = %v, want %v", names, tt.wantNames)
				}
			}
		})
	}
}
