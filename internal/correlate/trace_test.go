package correlate

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

type stubTraceFetcher struct {
	trace string
	err   error
}

func (s stubTraceFetcher) JobTrace(_ context.Context, _, _ int64) (string, error) {
	return s.trace, s.err
}

func TestTraceResolverPodForJob(t *testing.T) {
	tests := []struct {
		name    string
		trace   string
		wantPod string
		wantOK  bool
	}{
		{
			name:    "clean line",
			trace:   "Running on runner-5fbdek91-project-3-concurrent-1-tyqut4ic via gitlab-runner-79f44bb98f-n7bbw...\n",
			wantPod: "runner-5fbdek91-project-3-concurrent-1-tyqut4ic",
			wantOK:  true,
		},
		{
			name:    "ansi-wrapped line",
			trace:   "\x1b[0;m\x1b[0KRunning on runner-abc-project-7-concurrent-0 via mgr...\x1b[0;m\n",
			wantPod: "runner-abc-project-7-concurrent-0",
			wantOK:  true,
		},
		{
			name:    "non-SGR escape adjacent to line",
			trace:   "\x1b[0KRunning on runner-x-project-2-concurrent-0 via mgr...\x1b[0K\n",
			wantPod: "runner-x-project-2-concurrent-0",
			wantOK:  true,
		},
		{
			name:    "line not first, first match wins",
			trace:   "Preparing environment\nRunning on runner-a-project-1-concurrent-0 via m1...\nRunning on runner-b-project-1-concurrent-1 via m2...\n",
			wantPod: "runner-a-project-1-concurrent-0",
			wantOK:  true,
		},
		{
			name:   "no matching line",
			trace:  "Preparing environment\nJob succeeded\n",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewTraceResolver(stubTraceFetcher{trace: tt.trace}, zap.NewNop())
			pod, ok, err := r.PodForJob(context.Background(), 3, 101, time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("PodForJob: unexpected error %v", err)
			}
			if pod != tt.wantPod {
				t.Errorf("pod = %q, want %q", pod, tt.wantPod)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestTraceResolverFetchError(t *testing.T) {
	r := NewTraceResolver(stubTraceFetcher{err: errors.New("boom")}, zap.NewNop())
	_, _, err := r.PodForJob(context.Background(), 3, 101, time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("PodForJob: want error when JobTrace fails, got nil")
	}
}
