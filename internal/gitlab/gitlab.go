package gitlab

import (
	"context"
	"fmt"
	"io"
	"strings"

	gl "gitlab.com/gitlab-org/api/client-go"
	"go.uber.org/zap"
)

type apiClient struct {
	c   *gl.Client
	log *zap.Logger
}

// New returns a Client backed by the GitLab REST API.
func New(baseURL, token string, log *zap.Logger) (Client, error) {
	c, err := gl.NewClient(token, gl.WithBaseURL(baseURL))
	if err != nil {
		return nil, fmt.Errorf("create gitlab client: %w", err)
	}
	log.Debug("gitlab client created", zap.String("base_url", baseURL))

	return &apiClient{c: c, log: log}, nil
}

func (a *apiClient) PipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error) {
	opts := &gl.ListJobsOptions{ListOptions: gl.ListOptions{PerPage: 100}}
	var jobs []Job
	for {
		page, resp, err := a.c.Jobs.ListPipelineJobs(projectID, pipelineID, opts, gl.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("list jobs of pipeline %d: %w", pipelineID, err)
		}
		for _, j := range page {
			job := Job{ID: j.ID, Stage: j.Stage, Name: j.Name}
			if j.StartedAt != nil {
				job.StartedAt = *j.StartedAt
			}
			if j.FinishedAt != nil {
				job.FinishedAt = *j.FinishedAt
			}
			jobs = append(jobs, job)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	a.log.Debug("fetched pipeline jobs",
		zap.Int64("project_id", projectID), zap.Int64("pipeline_id", pipelineID), zap.Int("jobs", len(jobs)))
	return jobs, nil
}

func (a *apiClient) MergeRequestForBranch(ctx context.Context, projectID int64, branch string) (int64, bool, error) {
	opts := &gl.ListProjectMergeRequestsOptions{
		SourceBranch: gl.Ptr(branch),
		State:        gl.Ptr("opened"),
		ListOptions:  gl.ListOptions{PerPage: 1},
	}
	mrs, _, err := a.c.MergeRequests.ListProjectMergeRequests(projectID, opts, gl.WithContext(ctx))
	if err != nil {
		return 0, false, fmt.Errorf("list open MRs for branch %q: %w", branch, err)
	}
	if len(mrs) == 0 {
		a.log.Debug("no open MR for branch", zap.Int64("project_id", projectID), zap.String("branch", branch))
		return 0, false, nil
	}
	return mrs[0].IID, true, nil
}

func (a *apiClient) UpsertNote(ctx context.Context, projectID, mrIID int64, marker, body string) error {
	opts := &gl.ListMergeRequestNotesOptions{ListOptions: gl.ListOptions{PerPage: 100}}
	for {
		notes, resp, err := a.c.Notes.ListMergeRequestNotes(projectID, mrIID, opts, gl.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("list notes of MR !%d: %w", mrIID, err)
		}
		for _, n := range notes {
			if strings.Contains(n.Body, marker) {
				if _, _, err := a.c.Notes.UpdateMergeRequestNote(projectID, mrIID, n.ID,
					&gl.UpdateMergeRequestNoteOptions{Body: gl.Ptr(body)}, gl.WithContext(ctx)); err != nil {
					return fmt.Errorf("update note %d on MR !%d: %w", n.ID, mrIID, err)
				}
				a.log.Debug("updated existing MR note",
					zap.Int64("project_id", projectID), zap.Int64("mr_iid", mrIID), zap.Int64("note_id", n.ID))
				return nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if _, _, err := a.c.Notes.CreateMergeRequestNote(projectID, mrIID,
		&gl.CreateMergeRequestNoteOptions{Body: gl.Ptr(body)}, gl.WithContext(ctx)); err != nil {
		return fmt.Errorf("create note on MR !%d: %w", mrIID, err)
	}
	a.log.Debug("created new MR note", zap.Int64("project_id", projectID), zap.Int64("mr_iid", mrIID))
	return nil
}

func (a *apiClient) JobTrace(ctx context.Context, projectID, jobID int64) (string, error) {
	r, _, err := a.c.Jobs.GetTraceFile(projectID, jobID, gl.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("get trace of job %d: %w", jobID, err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read trace of job %d: %w", jobID, err)
	}
	a.log.Debug("fetched job trace",
		zap.Int64("project_id", projectID), zap.Int64("job_id", jobID), zap.Int("bytes", len(b)))
	return string(b), nil
}
