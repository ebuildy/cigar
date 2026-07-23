package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			iid, ok, err := c.MergeRequestForBranch(context.Background(), 7, tt.branch)
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
	ctx := context.Background()

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
