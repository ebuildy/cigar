package reporter

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

type fakeGitLab struct {
	jobs []gitlab.Job
	err  error
}

func (f *fakeGitLab) PipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error) {
	return f.jobs, f.err
}

func (f *fakeGitLab) MergeRequestForPipeline(context.Context, int64, int64) (int64, bool, error) {
	return 0, false, nil
}

func (f *fakeGitLab) UpsertNote(context.Context, int64, int64, string, string) error {
	return nil
}

type fakeResolver struct {
	pods map[int64]string // job ID -> pod name
	err  error
}

func (f *fakeResolver) PodForJob(_ context.Context, _, jobID int64, _, _ time.Time) (string, bool, error) {
	pod, ok := f.pods[jobID]
	return pod, ok, f.err
}

type fakeSource struct {
	usage map[string]*metrics.JobUsage // pod name -> usage
	err   error
}

func (f *fakeSource) PodUsage(_ context.Context, pod string, _, _ time.Time) (*metrics.JobUsage, error) {
	return f.usage[pod], f.err
}

func TestBuild(t *testing.T) {
	started := time.Now().Add(-5 * time.Minute)
	finished := time.Now()
	ranJob := gitlab.Job{ID: 1, Name: "test", StartedAt: started, FinishedAt: finished}

	tests := []struct {
		name      string
		gl        *fakeGitLab
		resolver  *fakeResolver
		source    *fakeSource
		wantErr   bool
		wantUsage []bool // per job, whether Usage must be non-nil
	}{
		{
			name:      "usage attached when pod correlates and metrics succeed",
			gl:        &fakeGitLab{jobs: []gitlab.Job{ranJob}},
			resolver:  &fakeResolver{pods: map[int64]string{1: "runner-pod-1"}},
			source:    &fakeSource{usage: map[string]*metrics.JobUsage{"runner-pod-1": {CPUSeconds: 12}}},
			wantUsage: []bool{true},
		},
		{
			name:      "job without pod keeps nil usage",
			gl:        &fakeGitLab{jobs: []gitlab.Job{ranJob}},
			resolver:  &fakeResolver{},
			source:    &fakeSource{},
			wantUsage: []bool{false},
		},
		{
			name:      "correlation error keeps nil usage",
			gl:        &fakeGitLab{jobs: []gitlab.Job{ranJob}},
			resolver:  &fakeResolver{err: errors.New("prom down")},
			source:    &fakeSource{},
			wantUsage: []bool{false},
		},
		{
			name:      "metrics error keeps nil usage",
			gl:        &fakeGitLab{jobs: []gitlab.Job{ranJob}},
			resolver:  &fakeResolver{pods: map[int64]string{1: "runner-pod-1"}},
			source:    &fakeSource{err: errors.New("query failed")},
			wantUsage: []bool{false},
		},
		{
			name:      "skipped job (no timestamps) is listed without usage",
			gl:        &fakeGitLab{jobs: []gitlab.Job{{ID: 2, Name: "manual"}}},
			resolver:  &fakeResolver{},
			source:    &fakeSource{},
			wantUsage: []bool{false},
		},
		{
			name:    "gitlab error fails the build",
			gl:      &fakeGitLab{err: errors.New("api down")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reporter{
				GitLab:            tt.gl,
				Resolver:          tt.resolver,
				Metrics:           tt.source,
				ThrottleWarnRatio: 0.25,
				Log:               slog.New(slog.DiscardHandler),
			}
			data, err := r.Build(context.Background(), 7, 42)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Build succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if data.PipelineID != 42 {
				t.Fatalf("PipelineID = %d, want 42", data.PipelineID)
			}
			if len(data.Jobs) != len(tt.wantUsage) {
				t.Fatalf("jobs = %d, want %d", len(data.Jobs), len(tt.wantUsage))
			}
			for i, want := range tt.wantUsage {
				if got := data.Jobs[i].Usage != nil; got != want {
					t.Fatalf("job %d usage present = %v, want %v", i, got, want)
				}
			}
		})
	}
}
