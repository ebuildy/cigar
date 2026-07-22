package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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

			c, err := New(srv.URL, "test-token")
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
