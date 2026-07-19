// Package reporter orchestrates building a pipeline resource report: jobs
// from GitLab, pod correlation, Prometheus usage, assembled into report.Data.
// It is shared by the webhook worker (posts an MR note) and the `bot run`
// CLI command (prints to stdout).
package reporter

import (
	"context"
	"fmt"
	"log/slog"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/correlate"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
)

type Reporter struct {
	GitLab            gitlab.Client
	Resolver          correlate.Resolver
	Metrics           metrics.Source
	ThrottleWarnRatio float64
	Log               *slog.Logger
}

// Build assembles the report data for one pipeline. Per-job failures
// (no pod correlated, metrics query failed) leave that job's Usage nil rather
// than failing the whole pipeline.
func (r *Reporter) Build(ctx context.Context, projectID, pipelineID int64) (report.Data, error) {
	jobs, err := r.GitLab.PipelineJobs(ctx, projectID, pipelineID)
	if err != nil {
		return report.Data{}, fmt.Errorf("list pipeline jobs: %w", err)
	}

	data := report.Data{PipelineID: pipelineID, ThrottleWarnRatio: r.ThrottleWarnRatio}
	for _, job := range jobs {
		data.Jobs = append(data.Jobs, report.JobReport{
			Name:  job.Name,
			Usage: r.jobUsage(ctx, projectID, job),
		})
	}
	return data, nil
}

func (r *Reporter) jobUsage(ctx context.Context, projectID int64, job gitlab.Job) *metrics.JobUsage {
	if job.StartedAt.IsZero() || job.FinishedAt.IsZero() {
		return nil // job never ran (skipped/canceled/manual)
	}
	pod, ok, err := r.Resolver.PodForJob(ctx, projectID, job.ID, job.StartedAt, job.FinishedAt)
	if err != nil {
		r.Log.Warn("pod correlation failed", "job", job.Name, "err", err)
		return nil
	}
	if !ok {
		r.Log.Warn("no runner pod found for job", "job", job.Name)
		return nil
	}
	usage, err := r.Metrics.PodUsage(ctx, pod, job.StartedAt, job.FinishedAt)
	if err != nil {
		r.Log.Warn("metrics query failed", "job", job.Name, "pod", pod, "err", err)
		return nil
	}
	return usage
}
