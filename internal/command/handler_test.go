package command

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
)

type fakeGitLab struct {
	discussion gitlab.Discussion
	jobs       []gitlab.Job
	replies    []string // reply bodies
	uploads    int
}

func (f *fakeGitLab) PipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error) {
	return f.jobs, nil
}
func (f *fakeGitLab) MergeRequestForBranch(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}
func (f *fakeGitLab) UpsertNote(context.Context, int64, int64, string, string) error { return nil }
func (f *fakeGitLab) JobTrace(context.Context, int64, int64) (string, error)         { return "", nil }
func (f *fakeGitLab) CurrentUser(context.Context) (int64, error)                     { return 555, nil }
func (f *fakeGitLab) MergeRequestDiscussion(context.Context, int64, int64, string) (gitlab.Discussion, error) {
	return f.discussion, nil
}
func (f *fakeGitLab) UploadFile(_ context.Context, _ int64, name string, _ []byte) (string, error) {
	f.uploads++
	return "![" + name + "](/uploads/x/" + name + ")", nil
}
func (f *fakeGitLab) CreateDiscussionReply(_ context.Context, _, _ int64, _, body string) error {
	f.replies = append(f.replies, body)
	return nil
}

type fakeResolver struct{ pods map[int64]string }

func (f *fakeResolver) PodForJob(_ context.Context, _, jobID int64, _, _ time.Time) (string, bool, error) {
	p, ok := f.pods[jobID]
	return p, ok, nil
}

type fakeSeries struct {
	series metrics.PodSeries
	spanOK bool
	spanS  time.Time
	spanE  time.Time
}

func (f *fakeSeries) PodSeries(context.Context, string, time.Time, time.Time) (metrics.PodSeries, error) {
	return f.series, nil
}
func (f *fakeSeries) PodActiveSpan(context.Context, string) (time.Time, time.Time, bool, error) {
	return f.spanS, f.spanE, f.spanOK, nil
}

const testKey = "test-key"

// signedRoot returns a discussion whose root note is the bot's signed report.
func signedRoot(pipelineID, mrIID int64) gitlab.Discussion {
	return gitlab.Discussion{
		RootNoteAuthorID: 555,
		RootNoteBody:     report.SignedMarker(pipelineID, mrIID, []byte(testKey)),
	}
}

func newHandler(gl *fakeGitLab, res *fakeResolver, se *fakeSeries) *Handler {
	return &Handler{
		GitLab: gl, Resolver: res, Series: se,
		SigningKey: []byte(testKey), BotUserID: 555, Log: zap.NewNop(),
	}
}

func TestHandleHelp(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3)}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "help"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gl.replies) != 1 || gl.replies[0] != HelpText {
		t.Fatalf("replies = %v, want one HelpText", gl.replies)
	}
}

func TestHandleIgnoresOwnNote(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3)}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	_ = h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 555, Body: "help"})
	if len(gl.replies) != 0 {
		t.Fatalf("replied to the bot's own note; replies=%v", gl.replies)
	}
}

func TestHandleRejectsNonBotRoot(t *testing.T) {
	d := signedRoot(42, 3)
	d.RootNoteAuthorID = 111
	gl := &fakeGitLab{discussion: d}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	_ = h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "help"})
	if len(gl.replies) != 0 {
		t.Fatal("replied in a thread whose root is not the bot's report")
	}
}

func TestHandleRejectsTamperedMarker(t *testing.T) {
	d := signedRoot(42, 3)
	d.RootNoteBody = "<!-- ci-resources-bot p=42 m=3 sig=deadbeef -->"
	gl := &fakeGitLab{discussion: d}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	_ = h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "help"})
	if len(gl.replies) != 0 {
		t.Fatal("acted on a tampered (bad-HMAC) report note")
	}
}

func TestHandleRejectsMRMismatch(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 999)}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	_ = h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "help"})
	if len(gl.replies) != 0 {
		t.Fatal("acted when the marker's MR did not match the event MR")
	}
}
