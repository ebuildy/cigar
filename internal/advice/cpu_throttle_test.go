package advice

import (
	"strings"
	"testing"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

func TestCPUThrottleFires(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	f := Facts{Name: "compile", Usage: &metrics.JobUsage{
		ThrottledRatio:  0.41,
		CPURequestCores: 0.25,
		CPULimitCores:   0.5,
	}}
	got := cpuThrottle{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	a := got[0]
	if a.Rule != "cpu-throttle" || a.Job != "compile" {
		t.Fatalf("advice = %+v, want rule cpu-throttle for job compile", a)
	}
	for _, want := range []string{"41%", "500m", "KUBERNETES_CPU_REQUEST", "KUBERNETES_CPU_LIMIT", "1000m"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body missing %q:\n%s", want, a.Body)
		}
	}
}

func TestCPUThrottleUnsetLimit(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	f := Facts{Name: "compile", Usage: &metrics.JobUsage{ThrottledRatio: 0.5}}
	got := cpuThrottle{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	// An absent limit series is reported as absent, never as a measured 0.
	if !strings.Contains(got[0].Body, "no CPU limit") {
		t.Errorf("body should say the limit series was absent:\n%s", got[0].Body)
	}
	if !strings.Contains(got[0].Body, `KUBERNETES_CPU_LIMIT: "1"`) {
		t.Errorf("body should suggest a 1-core limit when none is set:\n%s", got[0].Body)
	}
}

func TestCPUThrottleQuiet(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	tests := []struct {
		name  string
		facts Facts
	}{
		{name: "no usage", facts: Facts{Name: "compile"}},
		{name: "below threshold", facts: Facts{Name: "compile", Usage: &metrics.JobUsage{ThrottledRatio: 0.2}}},
		{name: "zero throttling", facts: Facts{Name: "compile", Usage: &metrics.JobUsage{}}},
	}
	rule := cpuThrottle{} // bound to a variable: a composite literal cannot
	// appear directly in an if/for header in Go.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rule.Check(tt.facts, th); got != nil {
				t.Fatalf("Check fired when it should not: %+v", got)
			}
		})
	}
}

// TestCPUThrottleRequestNeverExceedsLimit pins the Kubernetes constraint: a
// request above its limit is rejected, and requests/limits are summed per pod,
// so the measured pair is not guaranteed coherent.
func TestCPUThrottleRequestNeverExceedsLimit(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	f := Facts{Name: "compile", Usage: &metrics.JobUsage{
		ThrottledRatio:  0.9,
		CPURequestCores: 2, // request measured, limit series absent
	}}
	got := cpuThrottle{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	if strings.Contains(got[0].Body, `KUBERNETES_CPU_REQUEST: "2000m"`) {
		t.Errorf("suggested a request above the suggested limit:\n%s", got[0].Body)
	}
	if !strings.Contains(got[0].Body, `KUBERNETES_CPU_REQUEST: "1000m"`) {
		t.Errorf("request should be clamped to the suggested limit:\n%s", got[0].Body)
	}
}

func TestSuggestedCPULimit(t *testing.T) {
	tests := []struct {
		limit float64
		want  string
	}{
		{limit: 0, want: "1"},
		{limit: 0.5, want: "1000m"},
		{limit: 0.25, want: "500m"},
		{limit: 0.35, want: "700m"},
		{limit: 0.31, want: "700m"}, // 620m rounds up to the next 100m
		{limit: 2, want: "4000m"},
	}
	for _, tt := range tests {
		if got := suggestedCPULimit(tt.limit); got != tt.want {
			t.Errorf("suggestedCPULimit(%v) = %q, want %q", tt.limit, got, tt.want)
		}
	}
}
