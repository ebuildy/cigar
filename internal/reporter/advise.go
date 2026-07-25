package reporter

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
)

// Advise builds advice.Facts for every job of the pipeline — or the single job
// matching jobFilter (numeric ID or exact name) — and runs the engine over
// them. Jobs that never ran are skipped. Per-job metric failures leave Usage
// nil, so duration-based advice still applies; only the jobs listing failing
// aborts. An unmatched jobFilter returns an error wrapping
// gitlab.ErrJobNotFound.
func (r *Reporter) Advise(ctx context.Context, projectID, pipelineID int64, jobFilter string, eng *advice.Engine) ([]advice.Advice, error) {
	jobs, err := r.GitLab.PipelineJobs(ctx, projectID, pipelineID)
	if err != nil {
		return nil, fmt.Errorf("list pipeline jobs: %w", err)
	}
	if jobFilter != "" {
		j, ok := gitlab.FindJob(jobs, jobFilter)
		if !ok {
			return nil, fmt.Errorf("%q: %w", jobFilter, gitlab.ErrJobNotFound)
		}
		jobs = []gitlab.Job{j}
	}

	var out []advice.Advice
	for _, job := range jobs {
		if job.StartedAt.IsZero() || job.FinishedAt.IsZero() {
			continue // never ran (skipped/canceled/manual)
		}
		f := advice.Facts{
			Stage:    job.Stage,
			Name:     job.Name,
			Duration: job.FinishedAt.Sub(job.StartedAt),
			Usage:    r.jobUsage(ctx, projectID, job),
		}
		// Lazy: the trace costs a GitLab call, so fetch it only when a rule
		// actually wants it for this job.
		if eng.NeedsTrace(f) {
			trace, err := r.GitLab.JobTrace(ctx, projectID, job.ID)
			if err != nil {
				r.Log.Warn("fetch job trace for advice failed",
					zap.String("job", job.Name), zap.Int64("job_id", job.ID), zap.Error(err))
			} else {
				f.Trace = trace
			}
		}
		out = append(out, eng.Analyze(f)...)
	}
	r.Log.Debug("advice built",
		zap.Int64("pipeline_id", pipelineID), zap.Int("jobs", len(jobs)), zap.Int("advice", len(out)))
	return out, nil
}
