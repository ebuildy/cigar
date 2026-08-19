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

func TestCPUThrottlePerContainer(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	u := &metrics.JobUsage{Containers: []metrics.ContainerUsage{
		{Name: "build", ThrottledPeriods: 41, Periods: 100, CPURequestCores: 0.25, CPULimitCores: 0.5},
		{Name: "helper", ThrottledPeriods: 58, Periods: 100, CPURequestCores: 0.1, CPULimitCores: 0.25},
		{Name: "svc-0", ThrottledPeriods: 60, Periods: 100},
		{Name: "svc-1", ThrottledPeriods: 1, Periods: 100}, // below threshold, quiet
	}}
	u.SumForTest()
	got := cpuThrottle{}.Check(Facts{Name: "compile", Usage: u}, th)

	if len(got) != 3 {
		t.Fatalf("Check returned %d advice, want 3: %+v", len(got), got)
	}
	// Stable order, following the container order.
	wantTitles := []string{
		"⚠️ CPU throttling — build",
		"⚠️ CPU throttling — helper",
		"⚠️ CPU throttling — svc-0",
	}
	for i, want := range wantTitles {
		if got[i].Title != want {
			t.Errorf("advice %d title = %q, want %q", i, got[i].Title, want)
		}
		if got[i].Rule != "cpu-throttle" || got[i].Job != "compile" {
			t.Errorf("advice %d = %+v, want rule cpu-throttle for job compile", i, got[i])
		}
	}
	// Each container gets the variables that actually govern it.
	if !strings.Contains(got[0].Body, "KUBERNETES_CPU_LIMIT") ||
		strings.Contains(got[0].Body, "KUBERNETES_HELPER") {
		t.Errorf("build advice must use the build variables:\n%s", got[0].Body)
	}
	for _, want := range []string{"KUBERNETES_HELPER_CPU_REQUEST", "KUBERNETES_HELPER_CPU_LIMIT", "58%", "git clone"} {
		if !strings.Contains(got[1].Body, want) {
			t.Errorf("helper advice missing %q:\n%s", want, got[1].Body)
		}
	}
	for _, want := range []string{"KUBERNETES_SERVICE_CPU_REQUEST", "KUBERNETES_SERVICE_CPU_LIMIT", "every"} {
		if !strings.Contains(got[2].Body, want) {
			t.Errorf("service advice missing %q:\n%s", want, got[2].Body)
		}
	}
}

func TestCPUThrottleIgnoresContainersWithoutCFSSeries(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	u := &metrics.JobUsage{Containers: []metrics.ContainerUsage{{Name: "build", CPUSeconds: 5}}}
	u.SumForTest()
	if got := (cpuThrottle{}).Check(Facts{Name: "compile", Usage: u}, th); len(got) != 0 {
		t.Errorf("absent CFS series must not fire advice, got %+v", got)
	}
}

func TestCPUThrottleFallsBackWithoutBreakdown(t *testing.T) {
	// A Prometheus that cannot answer the container breakdown must produce
	// exactly the pre-breakdown advice.
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
	if got[0].Title != "⚠️ CPU throttling" {
		t.Errorf("title = %q, want the un-suffixed fallback title", got[0].Title)
	}
	if !strings.HasPrefix(got[0].Body, "This job spent **41%** of its CPU periods throttled, against a limit of 500m.") {
		t.Errorf("fallback body changed:\n%s", got[0].Body)
	}
}
