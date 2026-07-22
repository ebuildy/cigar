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
	Stage      string
	Name       string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Client is the boundary interface consumed by the worker; tests stub it.
type Client interface {
	// PipelineJobs returns all jobs of the given pipeline.
	PipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error)

	// MergeRequestForBranch resolves the open MR whose source branch is the
	// given ref. Used when a Pipeline webhook carries no merge_request yet
	// (branch pushed before the MR was created). ok is false when no open MR
	// exists for the branch (skip silently).
	MergeRequestForBranch(ctx context.Context, projectID int64, branch string) (iid int64, ok bool, err error)

	// UpsertNote creates the MR note, or updates the existing note that
	// contains marker (idempotent — one note per MR, never spam).
	UpsertNote(ctx context.Context, projectID, mrIID int64, marker, body string) error

	// JobTrace returns the raw trace log of a job.
	JobTrace(ctx context.Context, projectID, jobID int64) (string, error)
}
