package advice

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

var update = flag.Bool("update", false, "update golden files")

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	golden := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("output does not match %s (run with -update to refresh):\n%s", golden, got)
	}
}

// TestRenderGolden runs every rule across two jobs, so the golden file is the
// readable proof of what a user actually receives.
func TestRenderGolden(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25, LongJob: 10 * time.Minute, MemoryPressureRatio: 0.9}
	e, err := New(th, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var all []Advice
	all = append(all, e.Analyze(Facts{
		Stage:    "build",
		Name:     "compile",
		Duration: 22 * time.Minute,
		Trace:    "$ mvn -T 1C clean verify",
		Usage: &metrics.JobUsage{
			ThrottledRatio:     0.41,
			CPURequestCores:    0.25,
			CPULimitCores:      0.5,
			PeakMemoryBytes:    480 * 1024 * 1024,
			MemoryRequestBytes: 256 * 1024 * 1024,
			MemoryLimitBytes:   512 * 1024 * 1024,
		},
	})...)
	all = append(all, e.Analyze(Facts{
		Stage:    "test",
		Name:     "unit",
		Duration: 45 * time.Second,
		Usage: &metrics.JobUsage{
			ThrottledRatio:   0.02,
			CPULimitCores:    1,
			PeakMemoryBytes:  100 * 1024 * 1024,
			MemoryLimitBytes: 512 * 1024 * 1024,
		},
	})...)

	checkGolden(t, "advise.md", Render(12345, all))
}

func TestRenderCleanGolden(t *testing.T) {
	checkGolden(t, "advise-clean.md", Render(12345, nil))
}

func TestRenderCleanExactMessage(t *testing.T) {
	if got := Render(1, nil); got != "You are all good dude!" {
		t.Fatalf("Render with no advice = %q, want %q", got, "You are all good dude!")
	}
}

func TestRenderGroupsByJobInOrder(t *testing.T) {
	all := []Advice{
		{Job: "compile", Rule: "a", Title: "A", Body: "body a"},
		{Job: "unit", Rule: "b", Title: "B", Body: "body b"},
		{Job: "compile", Rule: "c", Title: "C", Body: "body c"},
	}
	got := Render(7, all)
	// Both of compile's findings must appear under one heading, before unit's.
	iCompile := indexOf(got, "#### `compile`")
	iUnit := indexOf(got, "#### `unit`")
	iA, iC := indexOf(got, "body a"), indexOf(got, "body c")
	if iCompile < 0 || iUnit < 0 {
		t.Fatalf("missing job headings in:\n%s", got)
	}
	grouped := iCompile < iA && iA < iC && iC < iUnit
	if !grouped {
		t.Fatalf("compile's findings are not grouped before unit's:\n%s", got)
	}
	if n := countOf(got, "#### `compile`"); n != 1 {
		t.Fatalf("compile heading appears %d times, want 1", n)
	}
}

func indexOf(s, sub string) int { return strings.Index(s, sub) }

func countOf(s, sub string) int { return strings.Count(s, sub) }
