package advice

import (
	"strings"
	"testing"
	"time"
)

func TestLongJobFires(t *testing.T) {
	th := Thresholds{LongJob: 10 * time.Minute}
	f := Facts{Name: "integration", Duration: 23*time.Minute + 30*time.Second}
	got := longJob{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	a := got[0]
	if a.Rule != "long-job" || a.Job != "integration" {
		t.Fatalf("advice = %+v, want rule long-job for job integration", a)
	}
	for _, want := range []string{"23m30s", "10m0s", "parallel"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body missing %q:\n%s", want, a.Body)
		}
	}
}

// TestLongJobFiresWithoutUsage proves duration-based advice survives a job
// whose runner pod never correlated — the only rule that works without metrics.
func TestLongJobFiresWithoutUsage(t *testing.T) {
	th := Thresholds{LongJob: 10 * time.Minute}
	got := longJob{}.Check(Facts{Name: "integration", Duration: 30 * time.Minute}, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1 (no Usage required)", len(got))
	}
}

func TestLongJobQuiet(t *testing.T) {
	tests := []struct {
		name  string
		th    Thresholds
		facts Facts
	}{
		{name: "under budget", th: Thresholds{LongJob: 10 * time.Minute}, facts: Facts{Duration: 3 * time.Minute}},
		{name: "exactly at budget", th: Thresholds{LongJob: 10 * time.Minute}, facts: Facts{Duration: 10 * time.Minute}},
		{name: "threshold disabled", th: Thresholds{LongJob: 0}, facts: Facts{Duration: 3 * time.Hour}},
	}
	rule := longJob{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rule.Check(tt.facts, tt.th); got != nil {
				t.Fatalf("Check fired when it should not: %+v", got)
			}
		})
	}
}
