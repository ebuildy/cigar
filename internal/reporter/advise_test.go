package reporter

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

// adviseGitLab is a gitlab.Client that serves a fixed job list and counts
// JobTrace calls, which is how we prove traces are fetched lazily.
type adviseGitLab struct {
	jobs        []gitlab.Job
	trace       string
	traceCalls  int
	tracedJobID int64
}

func (f *adviseGitLab) PipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error) {
	return f.jobs, nil
}
func (f *adviseGitLab) MergeRequestForBranch(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}
func (f *adviseGitLab) UpsertNote(context.Context, int64, int64, string, string) error { return nil }
func (f *adviseGitLab) JobTrace(_ context.Context, _, jobID int64) (string, error) {
	f.traceCalls++
	f.tracedJobID = jobID
	return f.trace, nil
}
func (f *adviseGitLab) CurrentUser(context.Context) (int64, error) { return 555, nil }
func (f *adviseGitLab) MergeRequestDiscussion(context.Context, int64, int64, string) (gitlab.Discussion, error) {
	return gitlab.Discussion{}, nil
}
func (f *adviseGitLab) UploadFile(context.Context, int64, string, []byte) (string, error) {
	return "", nil
}
func (f *adviseGitLab) CreateDiscussionReply(context.Context, int64, int64, string, string) error {
	return nil
}

type advisePods struct{ pods map[int64]string }

func (f *advisePods) PodForJob(_ context.Context, _, jobID int64, _, _ time.Time) (string, bool, error) {
	p, ok := f.pods[jobID]
	return p, ok, nil
}

type adviseMetrics struct{ usage map[string]*metrics.JobUsage }

func (f *adviseMetrics) PodUsage(_ context.Context, pod string, _, _ time.Time) (*metrics.JobUsage, error) {
	return f.usage[pod], nil
}

func adviseFixture(t *testing.T) (*Reporter, *adviseGitLab, *advice.Engine) {
	t.Helper()
	start := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	gl := &adviseGitLab{
		trace: "$ mvn -T 1C verify",
		jobs: []gitlab.Job{
			// Throttled: a trace consumer will ask for this one's trace.
			{ID: 1, Stage: "build", Name: "compile", StartedAt: start, FinishedAt: start.Add(20 * time.Minute)},
			// Healthy: no rule needs its trace.
			{ID: 2, Stage: "test", Name: "unit", StartedAt: start, FinishedAt: start.Add(time.Minute)},
			// Never ran: skipped entirely.
			{ID: 3, Stage: "deploy", Name: "staging"},
		},
	}
	r := &Reporter{
		GitLab:   gl,
		Resolver: &advisePods{pods: map[int64]string{1: "pod-1", 2: "pod-2"}},
		Metrics: &adviseMetrics{usage: map[string]*metrics.JobUsage{
			"pod-1": {ThrottledRatio: 0.8, CPULimitCores: 1, PeakMemoryBytes: 10, MemoryLimitBytes: 1000},
			"pod-2": {ThrottledRatio: 0.0, CPULimitCores: 1, PeakMemoryBytes: 10, MemoryLimitBytes: 1000},
		}},
		Log: zap.NewNop(),
	}
	eng, err := advice.New(advice.Thresholds{
		ThrottleWarnRatio:   0.25,
		LongJob:             10 * time.Minute,
		MemoryPressureRatio: 0.9,
	}, nil)
	if err != nil {
		t.Fatalf("advice.New: %v", err)
	}
	return r, gl, eng
}

func TestAdviseFetchesTraceOnlyForThrottledJobs(t *testing.T) {
	r, gl, eng := adviseFixture(t)

	all, err := r.Advise(t.Context(), 7, 42, "", eng)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	if gl.traceCalls != 1 {
		t.Fatalf("JobTrace called %d times, want exactly 1 (only the throttled job)", gl.traceCalls)
	}
	if gl.tracedJobID != 1 {
		t.Fatalf("traced job %d, want the throttled job 1", gl.tracedJobID)
	}

	rules := map[string]bool{}
	for _, a := range all {
		rules[a.Rule] = true
		if a.Job == "staging" {
			t.Fatal("a job that never ran produced advice")
		}
	}
	for _, want := range []string{"cpu-throttle", "java-threads", "long-job"} {
		if !rules[want] {
			t.Errorf("missing %s advice; got %v", want, rules)
		}
	}
}

func TestAdviseJobFilter(t *testing.T) {
	r, _, eng := adviseFixture(t)

	all, err := r.Advise(t.Context(), 7, 42, "unit", eng)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	for _, a := range all {
		if a.Job != "unit" {
			t.Fatalf("filter returned advice for %q", a.Job)
		}
	}

	if _, err := r.Advise(t.Context(), 7, 42, "nope", eng); !errors.Is(err, gitlab.ErrJobNotFound) {
		t.Fatalf("Advise with an unknown job = %v, want gitlab.ErrJobNotFound", err)
	}
}

// TestAdviseSurvivesMissingPod proves a job whose pod never correlated still
// gets duration-based advice instead of aborting the whole run.
func TestAdviseSurvivesMissingPod(t *testing.T) {
	r, _, eng := adviseFixture(t)
	r.Resolver = &advisePods{pods: map[int64]string{}} // nothing correlates

	all, err := r.Advise(t.Context(), 7, 42, "compile", eng)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	if len(all) != 1 || all[0].Rule != "long-job" {
		t.Fatalf("advice = %+v, want exactly the long-job finding", all)
	}
}
