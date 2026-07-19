// Package gitlab wraps the GitLab API: pipeline jobs listing, MR lookup and
// idempotent note create/update. The concrete implementation will use
// gitlab.com/gitlab-org/api/client-go.
package gitlab

import (
	"context"
	"time"
)

// Job is a finished CI job of a pipeline.
type Job struct {
	ID         int64
	Name       string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Client is the boundary interface consumed by the worker; tests stub it.
type Client interface {
	// PipelineJobs returns all jobs of the given pipeline.
	PipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error)

	// MergeRequestForPipeline resolves the MR the pipeline belongs to.
	// ok is false when the pipeline has no associated MR (skip silently).
	MergeRequestForPipeline(ctx context.Context, projectID, pipelineID int64) (iid int64, ok bool, err error)

	// UpsertNote creates the MR note, or updates the existing note that
	// contains marker (idempotent — one note per MR, never spam).
	UpsertNote(ctx context.Context, projectID, mrIID int64, marker, body string) error
}
