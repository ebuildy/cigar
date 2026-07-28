package gitlab

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestMergeRequestForBranch(t *testing.T) {
	tests := []struct {
		name     string
		branch   string
		respBody string
		wantIID  int64
		wantOK   bool
	}{
		{
			name:     "open MR found for branch",
			branch:   "feature-x",
			respBody: `[{"iid":9,"state":"opened","source_branch":"feature-x"}]`,
			wantIID:  9,
			wantOK:   true,
		},
		{
			name:     "no MR for branch",
			branch:   "feature-x",
			respBody: `[]`,
			wantIID:  0,
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotSourceBranch, gotState string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/projects/7/merge_requests" {
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				gotSourceBranch = r.URL.Query().Get("source_branch")
				gotState = r.URL.Query().Get("state")
				_, _ = w.Write([]byte(tt.respBody))
			}))
			defer srv.Close()

			c, err := New(srv.URL, "test-token", zap.NewNop())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			iid, ok, err := c.MergeRequestForBranch(t.Context(), 7, tt.branch)
			if err != nil {
				t.Fatalf("MergeRequestForBranch: %v", err)
			}
			if gotSourceBranch != tt.branch {
				t.Errorf("source_branch filter = %q, want %q", gotSourceBranch, tt.branch)
			}
			if gotState != "opened" {
				t.Errorf("state filter = %q, want %q", gotState, "opened")
			}
			if iid != tt.wantIID {
				t.Errorf("iid = %d, want %d", iid, tt.wantIID)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestNewClientMethods(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":555,"username":"cigar-bot"}`)
	})
	mux.HandleFunc("GET /api/v4/projects/7/merge_requests/3/discussions/abc",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id":"abc","notes":[{"id":1,"body":"report body","author":{"id":555}}]}`)
		})
	mux.HandleFunc("POST /api/v4/projects/7/uploads", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"markdown":"![cpu.svg](/uploads/deadbeef/cpu.svg)","url":"/uploads/deadbeef/cpu.svg"}`)
	})
	mux.HandleFunc("POST /api/v4/projects/7/merge_requests/3/discussions/abc/notes",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":2}`)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL, "tok", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := t.Context()

	uid, err := c.CurrentUser(ctx)
	if err != nil || uid != 555 {
		t.Fatalf("CurrentUser = (%d,%v), want (555,nil)", uid, err)
	}
	d, err := c.MergeRequestDiscussion(ctx, 7, 3, "abc")
	if err != nil {
		t.Fatalf("MergeRequestDiscussion: %v", err)
	}
	if d.RootNoteAuthorID != 555 || d.RootNoteBody != "report body" {
		t.Fatalf("discussion root = (%d,%q), want (555,'report body')", d.RootNoteAuthorID, d.RootNoteBody)
	}
	md, err := c.UploadFile(ctx, 7, "cpu.svg", []byte("<svg/>"))
	if err != nil || md == "" {
		t.Fatalf("UploadFile = (%q,%v)", md, err)
	}
	if err := c.CreateDiscussionReply(ctx, 7, 3, "abc", "hi"); err != nil {
		t.Fatalf("CreateDiscussionReply: %v", err)
	}
}

// TestUpsertNote pins the report-note policy: reuse the bot's existing report
// note only when its discussion has no sub-notes; once it has replies (e.g.
// from an advise/details command), post a fresh note instead of editing one
// people are already discussing.
func TestUpsertNote(t *testing.T) {
	const marker = "<!-- ci-resources-bot"
	reportRoot := func(id int, extra string) string {
		return fmt.Sprintf(`{"id":%d,"body":%q,"author":{"id":555},"system":false}`,
			id, marker+" p=1 m=1 sig=x -->\n"+extra)
	}
	plainNote := func(id int, body string) string {
		return fmt.Sprintf(`{"id":%d,"body":%q,"author":{"id":9},"system":false}`, id, body)
	}
	systemNote := func(id int, body string) string {
		return fmt.Sprintf(`{"id":%d,"body":%q,"author":{"id":1},"system":true}`, id, body)
	}
	disc := func(id string, notes ...string) string {
		return fmt.Sprintf(`{"id":%q,"individual_note":%t,"notes":[%s]}`,
			id, len(notes) == 1, strings.Join(notes, ","))
	}

	tests := []struct {
		name        string
		discussions string
		wantCreate  bool
		wantUpdate  int64 // note ID expected to be updated, 0 for none
	}{
		{name: "no report note creates", discussions: `[]`, wantCreate: true},
		{
			name:        "report note without replies is updated in place",
			discussions: "[" + disc("d1", reportRoot(10, "old")) + "]",
			wantUpdate:  10,
		},
		{
			name:        "report note with a reply gets a fresh note",
			discussions: "[" + disc("d1", reportRoot(10, "old"), plainNote(11, "an advise reply")) + "]",
			wantCreate:  true,
		},
		{
			name: "skips the replied report and updates the reply-less one",
			discussions: "[" +
				disc("d1", reportRoot(10, "old"), plainNote(11, "reply")) + "," +
				disc("d2", reportRoot(20, "newer")) + "]",
			wantUpdate: 20,
		},
		{
			name: "ignores human and system notes",
			discussions: "[" +
				disc("d0", plainNote(1, "just a human comment")) + "," +
				disc("d9", systemNote(9, "changed the description")) + "]",
			wantCreate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var created bool
			var updatedID int64
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v4/projects/7/merge_requests/3/discussions",
				func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tt.discussions) })
			mux.HandleFunc("POST /api/v4/projects/7/merge_requests/3/notes",
				func(w http.ResponseWriter, _ *http.Request) {
					created = true
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, `{"id":99}`)
				})
			mux.HandleFunc("PUT /api/v4/projects/7/merge_requests/3/notes/{id}",
				func(w http.ResponseWriter, r *http.Request) {
					updatedID, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
					_, _ = fmt.Fprintf(w, `{"id":%s}`, r.PathValue("id"))
				})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c, err := New(srv.URL, "tok", zap.NewNop())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := c.UpsertNote(t.Context(), 7, 3, marker, "new body"); err != nil {
				t.Fatalf("UpsertNote: %v", err)
			}
			if created != tt.wantCreate {
				t.Errorf("created = %v, want %v", created, tt.wantCreate)
			}
			if updatedID != tt.wantUpdate {
				t.Errorf("updated note = %d, want %d", updatedID, tt.wantUpdate)
			}
		})
	}
}

func TestRecentSuccessfulPipelines(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/7/pipelines", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = fmt.Fprint(w, `[
			{"id":900,"ref":"main","status":"success"},
			{"id":899,"ref":"refs/merge-requests/3/head","status":"success"},
			{"id":898,"ref":"feature-x","status":"success"}
		]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL, "tok", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.RecentSuccessfulPipelines(t.Context(), 7, 18)
	if err != nil {
		t.Fatalf("RecentSuccessfulPipelines: %v", err)
	}
	want := []Pipeline{
		{ID: 900, Ref: "main"},
		{ID: 899, Ref: "refs/merge-requests/3/head"},
		{ID: 898, Ref: "feature-x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pipelines = %+v, want %+v", got, want)
	}
	if !strings.Contains(gotQuery, "status=success") {
		t.Errorf("query %q does not filter status=success", gotQuery)
	}
	if !strings.Contains(gotQuery, "per_page=18") {
		t.Errorf("query %q does not request per_page=18", gotQuery)
	}
}

func TestRecentSuccessfulPipelinesStopsAtLimit(t *testing.T) {
	// GitLab may return a full page regardless of per_page; the client must not
	// hand back more than the caller asked for.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/7/pipelines", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[
			{"id":3,"ref":"a","status":"success"},
			{"id":2,"ref":"b","status":"success"},
			{"id":1,"ref":"c","status":"success"}
		]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := New(srv.URL, "tok", zap.NewNop())
	got, err := c.RecentSuccessfulPipelines(t.Context(), 7, 2)
	if err != nil {
		t.Fatalf("RecentSuccessfulPipelines: %v", err)
	}
	if len(got) != 2 || got[0].ID != 3 || got[1].ID != 2 {
		t.Errorf("got %+v, want the two newest", got)
	}
}

func TestPipelineRef(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/7/pipelines/42", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":42,"ref":"refs/merge-requests/9/head","status":"success"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := New(srv.URL, "tok", zap.NewNop())
	ref, err := c.PipelineRef(t.Context(), 7, 42)
	if err != nil {
		t.Fatalf("PipelineRef: %v", err)
	}
	if ref != "refs/merge-requests/9/head" {
		t.Errorf("ref = %q, want refs/merge-requests/9/head", ref)
	}
}
