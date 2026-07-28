package history

import (
	"testing"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
)

func TestMedian(t *testing.T) {
	tests := []struct {
		name string
		in   []time.Duration
		want time.Duration
		ok   bool
	}{
		{"odd count picks the middle", []time.Duration{
			3 * time.Minute, time.Minute, 2 * time.Minute,
		}, 2 * time.Minute, true},
		{"even count averages the two middles", []time.Duration{
			4 * time.Minute, time.Minute, 3 * time.Minute, 2 * time.Minute,
		}, 150 * time.Second, true},
		{"below minSamples yields nothing", []time.Duration{
			time.Minute, 2 * time.Minute,
		}, 0, false},
		{"empty yields nothing", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := newStat(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if got.Median != tt.want {
				t.Errorf("median = %s, want %s", got.Median, tt.want)
			}
			if got.Samples != len(tt.in) {
				t.Errorf("samples = %d, want %d", got.Samples, len(tt.in))
			}
		})
	}
}

func TestNewStatDoesNotMutateInput(t *testing.T) {
	in := []time.Duration{3 * time.Minute, time.Minute, 2 * time.Minute}
	newStat(in)
	if in[0] != 3*time.Minute {
		t.Errorf("input was sorted in place: %v", in)
	}
}

func TestPipelineWallClock(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	jobs := []gitlab.Job{
		{Stage: "build", Name: "compile", StartedAt: base, FinishedAt: base.Add(2 * time.Minute)},
		{Stage: "test", Name: "unit", StartedAt: base.Add(time.Minute), FinishedAt: base.Add(5 * time.Minute)},
		{Stage: "deploy", Name: "staging"}, // never ran
	}
	got, ok := pipelineWallClock(jobs)
	if !ok {
		t.Fatal("ok = false, want a wall clock")
	}
	if got != 5*time.Minute {
		t.Errorf("wall clock = %s, want 5m (max finish - min start)", got)
	}

	if _, ok := pipelineWallClock([]gitlab.Job{{Stage: "s", Name: "n"}}); ok {
		t.Error("a pipeline with no run window must not produce a wall clock")
	}
}

func TestJobDurationsKeepsLastFinishingRetry(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	jobs := []gitlab.Job{
		// First attempt: 1m. Retry: 3m, finishing later — the retry wins.
		{Stage: "test", Name: "unit", StartedAt: base, FinishedAt: base.Add(time.Minute)},
		{Stage: "test", Name: "unit", StartedAt: base.Add(2 * time.Minute), FinishedAt: base.Add(5 * time.Minute)},
		{Stage: "deploy", Name: "staging"}, // never ran, contributes nothing
	}
	got := jobDurations(jobs)
	if len(got) != 1 {
		t.Fatalf("got %d job keys, want 1: %+v", len(got), got)
	}
	if d := got[JobKey{Stage: "test", Name: "unit"}]; d != 3*time.Minute {
		t.Errorf("duration = %s, want 3m (the last-finishing attempt)", d)
	}
}

func TestSelectSamplesExcludesRefs(t *testing.T) {
	all := []gitlab.Pipeline{
		{ID: 10, Ref: "feature-x"},
		{ID: 9, Ref: "main"},
		{ID: 8, Ref: "refs/merge-requests/3/head"},
		{ID: 7, Ref: "main"},
		{ID: 6, Ref: "other"},
	}
	got := selectSamples(all, []string{"feature-x", "refs/merge-requests/3/head"}, 3)
	want := []int64{9, 7, 6}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want ids %v", got, want)
	}
	for i, p := range got {
		if p.ID != want[i] {
			t.Errorf("sample %d = %d, want %d", i, p.ID, want[i])
		}
	}

	// Newest-first order is preserved and the limit truncates the tail.
	if got := selectSamples(all, nil, 2); len(got) != 2 || got[0].ID != 10 || got[1].ID != 9 {
		t.Errorf("got %+v, want the two newest", got)
	}
	// No exclusions and no limit slack: an empty excludeRefs filters nothing.
	if got := selectSamples(all, []string{}, 10); len(got) != 5 {
		t.Errorf("got %d samples, want all 5", len(got))
	}
}

func TestReduceBuildsMedians(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	// Three pipelines: wall clocks 2m, 4m, 6m -> median 4m.
	// "build : compile" runs in all three (1m, 2m, 3m -> 2m);
	// "test : flaky" runs in only two, so it must produce no Stat.
	mk := func(wall, compile time.Duration, withFlaky bool) []gitlab.Job {
		jobs := []gitlab.Job{
			{Stage: "build", Name: "compile", StartedAt: base, FinishedAt: base.Add(compile)},
			{Stage: "e2e", Name: "run", StartedAt: base, FinishedAt: base.Add(wall)},
		}
		if withFlaky {
			jobs = append(jobs, gitlab.Job{
				Stage: "test", Name: "flaky", StartedAt: base, FinishedAt: base.Add(time.Minute),
			})
		}
		return jobs
	}
	b := reduce([][]gitlab.Job{
		mk(2*time.Minute, time.Minute, true),
		mk(4*time.Minute, 2*time.Minute, false),
		mk(6*time.Minute, 3*time.Minute, true),
	})

	if b.Pipeline.Median != 4*time.Minute || b.Pipeline.Samples != 3 {
		t.Errorf("pipeline stat = %+v, want median 4m over 3 samples", b.Pipeline)
	}
	compile, ok := b.Jobs[JobKey{Stage: "build", Name: "compile"}]
	if !ok || compile.Median != 2*time.Minute || compile.Samples != 3 {
		t.Errorf("compile stat = %+v (ok=%v), want median 2m over 3 samples", compile, ok)
	}
	if _, ok := b.Jobs[JobKey{Stage: "test", Name: "flaky"}]; ok {
		t.Error("a job seen in only 2 pipelines must not get a Stat")
	}
}
