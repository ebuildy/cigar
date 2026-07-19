package gitlab

import (
	"context"
	"fmt"
	"strings"

	gl "gitlab.com/gitlab-org/api/client-go"
)

type apiClient struct {
	c *gl.Client
}

// New returns a Client backed by the GitLab REST API.
func New(baseURL, token string) (Client, error) {
	c, err := gl.NewClient(token, gl.WithBaseURL(baseURL))
	if err != nil {
		return nil, fmt.Errorf("create gitlab client: %w", err)
	}
	return &apiClient{c: c}, nil
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
			job := Job{ID: j.ID, Name: j.Name}
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
	return jobs, nil
}

func (a *apiClient) MergeRequestForPipeline(ctx context.Context, projectID, pipelineID int64) (int64, bool, error) {
	p, _, err := a.c.Pipelines.GetPipeline(projectID, pipelineID, gl.WithContext(ctx))
	if err != nil {
		return 0, false, fmt.Errorf("get pipeline %d: %w", pipelineID, err)
	}
	mrs, _, err := a.c.Commits.ListMergeRequestsByCommit(projectID, p.SHA, gl.WithContext(ctx))
	if err != nil {
		return 0, false, fmt.Errorf("list MRs for commit %s: %w", p.SHA, err)
	}
	for _, mr := range mrs {
		if mr.State == "opened" {
			return mr.IID, true, nil
		}
	}
	return 0, false, nil
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
	return nil
}
