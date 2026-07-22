// Package reporter orchestrates building a pipeline resource report: jobs
// from GitLab, pod correlation, Prometheus usage, assembled into report.Data.
// It is shared by the webhook worker (posts an MR note) and the `bot run`
// CLI command (prints to stdout).
package reporter

import (
	"context"
	"fmt"

	"go.uber.org/zap"

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
	Log               *zap.Logger
}

// ProcessPipeline builds the report for one pipeline and upserts it as a
// note on the MR. When mrIID is 0 (the webhook had no merge_request yet,
// e.g. the branch was pushed before the MR was created) it resolves the
// open MR from the branch ref; if none exists it returns posted=false and
// does nothing. This is the whole webhook-worker path; `bot run` uses
// Build + report.Render directly to print instead of posting.
func (r *Reporter) ProcessPipeline(ctx context.Context, projectID, pipelineID, mrIID int64, ref, status string) (bool, error) {
	if mrIID == 0 {
		// The webhook carried no merge_request (branch pushed before the MR
		// was created). Resolve an open MR from the pipeline's branch ref.
		iid, ok, err := r.GitLab.MergeRequestForBranch(ctx, projectID, ref)
		if err != nil {
			return false, fmt.Errorf("resolve MR for branch %q: %w", ref, err)
		}
		if !ok {
			r.Log.Info("no open merge request for branch yet, skipping",
				zap.Int64("project_id", projectID), zap.String("branch", ref), zap.Int64("pipeline_id", pipelineID))
			return false, nil
		}
		r.Log.Debug("resolved open merge request from branch",
			zap.Int64("project_id", projectID), zap.String("branch", ref), zap.Int64("mr_iid", iid))
		mrIID = iid
	}

	data, err := r.Build(ctx, projectID, pipelineID)
	if err != nil {
		return false, err
	}
	data.Status = status

	body, err := report.Render(data)
	if err != nil {
		return false, fmt.Errorf("render report: %w", err)
	}
	if err := r.GitLab.UpsertNote(ctx, projectID, mrIID, report.Marker, body); err != nil {
		return false, fmt.Errorf("upsert note on MR !%d: %w", mrIID, err)
	}
	r.Log.Info("upserted resource report note",
		zap.Int64("project_id", projectID), zap.Int64("mr_iid", mrIID), zap.Int("body_bytes", len(body)))
	return true, nil
}

// Build assembles the report data for one pipeline. Per-job failures
// (no pod correlated, metrics query failed) leave that job's Usage nil rather
// than failing the whole pipeline.
func (r *Reporter) Build(ctx context.Context, projectID, pipelineID int64) (report.Data, error) {
	jobs, err := r.GitLab.PipelineJobs(ctx, projectID, pipelineID)
	if err != nil {
		return report.Data{}, fmt.Errorf("list pipeline jobs: %w", err)
	}
	r.Log.Debug("listed pipeline jobs",
		zap.Int64("project_id", projectID), zap.Int64("pipeline_id", pipelineID), zap.Int("jobs", len(jobs)))

	data := report.Data{PipelineID: pipelineID, ThrottleWarnRatio: r.ThrottleWarnRatio}
	for _, job := range jobs {
		data.Jobs = append(data.Jobs, report.JobReport{
			Stage: job.Stage,
			Name:  job.Name,
			Usage: r.jobUsage(ctx, projectID, job),
		})
	}
	return data, nil
}

func (r *Reporter) jobUsage(ctx context.Context, projectID int64, job gitlab.Job) *metrics.JobUsage {
	if job.StartedAt.IsZero() || job.FinishedAt.IsZero() {
		r.Log.Debug("job never ran, skipping usage", zap.String("job", job.Name))
		return nil // job never ran (skipped/canceled/manual)
	}
	pod, ok, err := r.Resolver.PodForJob(ctx, projectID, job.ID, job.StartedAt, job.FinishedAt)
	if err != nil {
		r.Log.Warn("pod correlation failed", zap.String("job", job.Name), zap.Error(err))
		return nil
	}
	if !ok {
		r.Log.Warn("no runner pod found for job", zap.String("job", job.Name))
		return nil
	}
	r.Log.Debug("correlated job to runner pod", zap.String("job", job.Name), zap.String("pod", pod))
	usage, err := r.Metrics.PodUsage(ctx, pod, job.StartedAt, job.FinishedAt)
	if err != nil {
		r.Log.Warn("metrics query failed", zap.String("job", job.Name), zap.String("pod", pod), zap.Error(err))
		return nil
	}
	return usage
}
