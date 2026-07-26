package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/telemetry"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/webhook"
)

func TestQueueWarnsWhenNearlyFull(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	q := newQueue(10, zap.New(core)) // 80% of 10 => warn from depth 8

	if q.warnAt != 8 {
		t.Fatalf("warnAt = %d, want 8", q.warnAt)
	}

	// Below the threshold: silence.
	for range 7 {
		if !q.Enqueue(webhook.Event{}) {
			t.Fatal("Enqueue returned false below capacity")
		}
	}
	if n := logs.FilterMessage("worker queue nearly full").Len(); n != 0 {
		t.Fatalf("warned at depth 7: %d entries", n)
	}

	// Crossing it warns exactly once, however far past it we go.
	for range 3 {
		if !q.Enqueue(webhook.Event{}) {
			t.Fatal("Enqueue returned false below capacity")
		}
	}
	entries := logs.FilterMessage("worker queue nearly full").All()
	if len(entries) != 1 {
		t.Fatalf("warned %d times crossing the threshold, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["depth"] != int64(8) || fields["capacity"] != int64(10) {
		t.Fatalf("warning fields = %v, want depth 8 capacity 10", fields)
	}

	// Full: Enqueue reports the drop to the caller (which logs it with the
	// event's IDs) rather than blocking.
	if q.Enqueue(webhook.Event{}) {
		t.Fatal("Enqueue returned true on a full queue")
	}

	// The warning stays armed until an enqueue observes the queue back under
	// the threshold; the next climb past it warns again.
	for range 4 {
		<-q.ch // depth 10 -> 6
	}
	if !q.Enqueue(webhook.Event{}) { // depth 7: recovered, re-arms, no warning
		t.Fatal("Enqueue returned false after draining")
	}
	if n := logs.FilterMessage("worker queue nearly full").Len(); n != 1 {
		t.Fatalf("warned %d times while recovered, want 1", n)
	}
	if !q.Enqueue(webhook.Event{}) { // depth 8: crosses again
		t.Fatal("Enqueue returned false after draining")
	}
	if n := logs.FilterMessage("worker queue nearly full").Len(); n != 2 {
		t.Fatalf("warned %d times total, want 2 (one per crossing)", n)
	}
}

// The queue warning must reach cigar_log_total with the subsystem name serve
// wires it under — proving the LogOption + Named chain, not just the core.
func TestQueueWarningIsCounted(t *testing.T) {
	m := telemetry.New()
	// A nop logger: nothing is printed, yet the entry is still counted.
	q := newQueue(2, zap.NewNop().WithOptions(m.LogOption()).Named("queue"))

	q.Enqueue(webhook.Event{})
	q.Enqueue(webhook.Event{}) // depth 2 of 2 => nearly full

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Result().Body)

	const want = `cigar_log_total{level="warn",name="queue"} 1`
	if !strings.Contains(string(body), want) {
		t.Errorf("metrics output missing %s, got:\n%s", want, body)
	}
}

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
