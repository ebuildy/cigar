package gitlab

import (
	"bytes"
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

// RecentSuccessfulPipelines lists the project's successful pipelines newest
// first (GitLab's default id-descending order), paginating only far enough to
// reach limit.
func (a *apiClient) RecentSuccessfulPipelines(ctx context.Context, projectID int64, limit int) ([]Pipeline, error) {
	if limit <= 0 {
		return nil, nil
	}
	opts := &gl.ListProjectPipelinesOptions{
		Status:      new(gl.Success),
		ListOptions: gl.ListOptions{PerPage: int64(min(limit, 100))},
	}
	var out []Pipeline
	for {
		page, resp, err := a.c.Pipelines.ListProjectPipelines(projectID, opts, gl.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("list successful pipelines of project %d: %w", projectID, err)
		}
		for _, p := range page {
			out = append(out, Pipeline{ID: p.ID, Ref: p.Ref})
		}
		// A page can exceed per_page; stop once limit is covered and trim below.
		if len(out) >= limit || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	out = out[:min(len(out), limit)]
	a.log.Debug("fetched recent successful pipelines",
		zap.Int64("project_id", projectID), zap.Int("pipelines", len(out)))
	return out, nil
}

func (a *apiClient) PipelineRef(ctx context.Context, projectID, pipelineID int64) (string, error) {
	p, _, err := a.c.Pipelines.GetPipeline(projectID, pipelineID, gl.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("get pipeline %d: %w", pipelineID, err)
	}
	return p.Ref, nil
}

func (a *apiClient) MergeRequestForBranch(ctx context.Context, projectID int64, branch string) (int64, bool, error) {
	opts := &gl.ListProjectMergeRequestsOptions{
		SourceBranch: new(branch),
		State:        new("opened"),
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

// UpsertNote posts the report, reusing the bot's existing report note only when
// its discussion has no sub-notes. A report note whose thread has grown replies
// (e.g. from an interactive advise/details command) is left untouched and a
// fresh note is posted instead — the running report must never edit a note
// people are already discussing.
func (a *apiClient) UpsertNote(ctx context.Context, projectID, mrIID int64, marker, body string) error {
	opts := &gl.ListMergeRequestDiscussionsOptions{ListOptions: gl.ListOptions{PerPage: 100}}
	for {
		discs, resp, err := a.c.Discussions.ListMergeRequestDiscussions(projectID, mrIID, opts, gl.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("list discussions of MR !%d: %w", mrIID, err)
		}
		for _, d := range discs {
			if len(d.Notes) == 0 {
				continue
			}
			root := d.Notes[0]
			// A report note is the marker-bearing root of its thread. Skip
			// human/system notes, and skip any report thread that already has
			// replies (len > 1) — that one gets a new note, not an edit.
			if root.System || !strings.Contains(root.Body, marker) || len(d.Notes) > 1 {
				continue
			}
			if _, _, err := a.c.Notes.UpdateMergeRequestNote(projectID, mrIID, root.ID,
				&gl.UpdateMergeRequestNoteOptions{Body: new(body)}, gl.WithContext(ctx)); err != nil {
				return fmt.Errorf("update note %d on MR !%d: %w", root.ID, mrIID, err)
			}
			a.log.Debug("updated existing MR report note in place",
				zap.Int64("project_id", projectID), zap.Int64("mr_iid", mrIID), zap.Int64("note_id", root.ID))
			return nil
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if _, _, err := a.c.Notes.CreateMergeRequestNote(projectID, mrIID,
		&gl.CreateMergeRequestNoteOptions{Body: new(body)}, gl.WithContext(ctx)); err != nil {
		return fmt.Errorf("create note on MR !%d: %w", mrIID, err)
	}
	a.log.Debug("created new MR report note", zap.Int64("project_id", projectID), zap.Int64("mr_iid", mrIID))
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

func (a *apiClient) CurrentUser(ctx context.Context) (int64, error) {
	u, _, err := a.c.Users.CurrentUser(gl.WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("get current user: %w", err)
	}
	return u.ID, nil
}

func (a *apiClient) MergeRequestDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string) (Discussion, error) {
	d, _, err := a.c.Discussions.GetMergeRequestDiscussion(projectID, mrIID, discussionID, gl.WithContext(ctx))
	if err != nil {
		return Discussion{}, fmt.Errorf("get discussion %s on MR !%d: %w", discussionID, mrIID, err)
	}
	if len(d.Notes) == 0 {
		return Discussion{}, fmt.Errorf("discussion %s has no notes", discussionID)
	}
	root := d.Notes[0]
	return Discussion{RootNoteAuthorID: root.Author.ID, RootNoteBody: root.Body}, nil
}

func (a *apiClient) UploadFile(ctx context.Context, projectID int64, filename string, content []byte) (string, error) {
	f, _, err := a.c.ProjectMarkdownUploads.UploadProjectMarkdown(projectID, bytes.NewReader(content), filename, gl.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("upload %q to project %d: %w", filename, projectID, err)
	}
	return f.Markdown, nil
}

func (a *apiClient) CreateDiscussionReply(ctx context.Context, projectID, mrIID int64, discussionID, body string) error {
	_, _, err := a.c.Discussions.AddMergeRequestDiscussionNote(projectID, mrIID, discussionID,
		&gl.AddMergeRequestDiscussionNoteOptions{Body: new(body)}, gl.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("reply in discussion %s on MR !%d: %w", discussionID, mrIID, err)
	}
	return nil
}
