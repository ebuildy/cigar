package advice

import (
	"strings"
	"testing"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

const mib = 1024 * 1024

func TestMemoryPressureFires(t *testing.T) {
	th := Thresholds{MemoryPressureRatio: 0.9}
	f := Facts{Name: "unit", Usage: &metrics.JobUsage{
		PeakMemoryBytes:  480 * mib,
		MemoryLimitBytes: 512 * mib,
	}}
	got := memoryPressure{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	a := got[0]
	if a.Rule != "memory-pressure" || a.Job != "unit" {
		t.Fatalf("advice = %+v, want rule memory-pressure for job unit", a)
	}
	for _, want := range []string{"94%", "480Mi", "512Mi", "KUBERNETES_MEMORY_LIMIT", "OOMKill"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body missing %q:\n%s", want, a.Body)
		}
	}
}

// TestMemoryPressureAbsentLimit pins the "absent != zero" rule: with no limit
// series there is no denominator, so the rule must stay silent rather than
// treat 0 as a limit the job exceeded.
func TestMemoryPressureAbsentLimit(t *testing.T) {
	th := Thresholds{MemoryPressureRatio: 0.9}
	f := Facts{Name: "unit", Usage: &metrics.JobUsage{PeakMemoryBytes: 900 * mib}}
	rule := memoryPressure{}
	if got := rule.Check(f, th); got != nil {
		t.Fatalf("Check fired without a limit series: %+v", got)
	}
}

// TestMemoryPressureBoundaries covers the two ends the guard clause turns on:
// exactly at the threshold it fires, and a peak that already passed the limit
// (a late-arriving sample before an OOMKill) still fires and still clamps.
func TestMemoryPressureBoundaries(t *testing.T) {
	th := Thresholds{MemoryPressureRatio: 0.9}
	rule := memoryPressure{}

	atThreshold := rule.Check(Facts{Name: "unit", Usage: &metrics.JobUsage{
		PeakMemoryBytes:  461 * mib, // 461/512 = 0.900…, the first ratio at 0.9
		MemoryLimitBytes: 512 * mib,
	}}, th)
	if len(atThreshold) != 1 {
		t.Fatalf("at the threshold: %d advice, want 1", len(atThreshold))
	}

	overLimit := rule.Check(Facts{Name: "unit", Usage: &metrics.JobUsage{
		PeakMemoryBytes:  600 * mib,
		MemoryLimitBytes: 512 * mib,
	}}, th)
	if len(overLimit) != 1 {
		t.Fatalf("peak over limit: %d advice, want 1", len(overLimit))
	}
	if !strings.Contains(overLimit[0].Body, "117%") {
		t.Errorf("peak over limit should report a ratio above 100%%:\n%s", overLimit[0].Body)
	}
}

func TestMemoryPressureQuiet(t *testing.T) {
	th := Thresholds{MemoryPressureRatio: 0.9}
	tests := []struct {
		name  string
		facts Facts
	}{
		{name: "no usage", facts: Facts{Name: "unit"}},
		{name: "well under limit", facts: Facts{Name: "unit", Usage: &metrics.JobUsage{
			PeakMemoryBytes: 100 * mib, MemoryLimitBytes: 512 * mib,
		}}},
	}
	rule := memoryPressure{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rule.Check(tt.facts, th); got != nil {
				t.Fatalf("Check fired when it should not: %+v", got)
			}
		})
	}
}

// TestMemoryPressureRequestNeverExceedsLimit pins the Kubernetes constraint:
// requests and limits are summed per pod, so the measured pair is not
// guaranteed coherent, and a request above its limit is rejected outright.
func TestMemoryPressureRequestNeverExceedsLimit(t *testing.T) {
	th := Thresholds{MemoryPressureRatio: 0.9}
	f := Facts{Name: "unit", Usage: &metrics.JobUsage{
		PeakMemoryBytes:    480 * mib,
		MemoryLimitBytes:   512 * mib,
		MemoryRequestBytes: 4096 * mib, // wildly above the suggested 768Mi limit
	}}
	got := memoryPressure{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	if strings.Contains(got[0].Body, `KUBERNETES_MEMORY_REQUEST: "4096Mi"`) {
		t.Errorf("suggested a request above the suggested limit:\n%s", got[0].Body)
	}
	if !strings.Contains(got[0].Body, `KUBERNETES_MEMORY_REQUEST: "768Mi"`) {
		t.Errorf("request should be clamped to the suggested limit:\n%s", got[0].Body)
	}
}

func TestSuggestedMemory(t *testing.T) {
	tests := []struct {
		peak uint64
		want string
	}{
		{peak: 480 * mib, want: "768Mi"},   // 720Mi rounded up to the next 128Mi
		{peak: 100 * mib, want: "256Mi"},   // 150Mi rounded up
		{peak: 1024 * mib, want: "1536Mi"}, // already a multiple of 128Mi
		{peak: 200 * mib, want: "384Mi"},   // 300Mi rounds up to 384Mi
	}
	for _, tt := range tests {
		if got := suggestedMemory(tt.peak); got != tt.want {
			t.Errorf("suggestedMemory(%d) = %q, want %q", tt.peak, got, tt.want)
		}
	}
}
