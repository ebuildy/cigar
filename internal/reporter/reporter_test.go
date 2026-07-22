package reporter

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

type fakeGitLab struct {
	jobs []gitlab.Job
	err  error

	branchMR map[string]int64 // source branch -> open MR IID

	upsertedMR int64 // MR IID of the last UpsertNote call
	upserts    int
}

func (f *fakeGitLab) PipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error) {
	return f.jobs, f.err
}

func (f *fakeGitLab) MergeRequestForBranch(_ context.Context, _ int64, branch string) (int64, bool, error) {
	iid, ok := f.branchMR[branch]
	return iid, ok, nil
}

func (f *fakeGitLab) UpsertNote(_ context.Context, _, mrIID int64, _, _ string) error {
	f.upsertedMR = mrIID
	f.upserts++
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

func TestProcessPipeline(t *testing.T) {
	job := gitlab.Job{ID: 1, Stage: "build", Name: "compile"}

	tests := []struct {
		name       string
		mrIID      int64
		ref        string
		branchMR   map[string]int64
		wantPosted bool
		wantMR     int64 // MR the note must be upserted on (when posted)
	}{
		{
			name:       "posts to the MR from the webhook payload",
			mrIID:      3,
			ref:        "feature-x",
			wantPosted: true,
			wantMR:     3,
		},
		{
			name:       "resolves the open MR from the branch when payload has none",
			mrIID:      0,
			ref:        "feature-x",
			branchMR:   map[string]int64{"feature-x": 9},
			wantPosted: true,
			wantMR:     9,
		},
		{
			name:       "skips when no open MR exists for the branch yet",
			mrIID:      0,
			ref:        "feature-x",
			branchMR:   map[string]int64{},
			wantPosted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gl := &fakeGitLab{jobs: []gitlab.Job{job}, branchMR: tt.branchMR}
			r := &Reporter{
				GitLab:   gl,
				Resolver: &fakeResolver{},
				Metrics:  &fakeSource{},
				Log:      zap.NewNop(),
			}
			posted, err := r.ProcessPipeline(context.Background(), 7, 42, tt.mrIID, tt.ref, "success")
			if err != nil {
				t.Fatalf("ProcessPipeline: %v", err)
			}
			if posted != tt.wantPosted {
				t.Fatalf("posted = %v, want %v", posted, tt.wantPosted)
			}
			if tt.wantPosted {
				if gl.upserts != 1 {
					t.Fatalf("upserts = %d, want 1", gl.upserts)
				}
				if gl.upsertedMR != tt.wantMR {
					t.Fatalf("note upserted on MR !%d, want !%d", gl.upsertedMR, tt.wantMR)
				}
			} else if gl.upserts != 0 {
				t.Fatalf("upserts = %d, want 0 (nothing to post)", gl.upserts)
			}
		})
	}
}

func TestBuildMapsStageAndName(t *testing.T) {
	gl := &fakeGitLab{jobs: []gitlab.Job{{ID: 1, Stage: "build", Name: "compile"}}}
	r := &Reporter{
		GitLab:   gl,
		Resolver: &fakeResolver{},
		Metrics:  &fakeSource{},
		Log:      zap.NewNop(),
	}
	data, err := r.Build(context.Background(), 7, 42)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := data.Jobs[0].Stage; got != "build" {
		t.Errorf("Stage = %q, want %q", got, "build")
	}
	if got := data.Jobs[0].Name; got != "compile" {
		t.Errorf("Name = %q, want %q", got, "compile")
	}
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
				Log:               zap.NewNop(),
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
