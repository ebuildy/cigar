package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestJobTrace(t *testing.T) {
	const trace = "Running on runner-abc-project-7-concurrent-0-xyz via gitlab-runner-1...\nDone\n"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(trace))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "test-token", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.JobTrace(context.Background(), 7, 101)
	if err != nil {
		t.Fatalf("JobTrace: %v", err)
	}
	if want := "/api/v4/projects/7/jobs/101/trace"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if got != trace {
		t.Errorf("trace = %q, want %q", got, trace)
	}
}
