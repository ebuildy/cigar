package history

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
)

// stubGitLab counts calls so cache hits can be proven by their absence.
type stubGitLab struct {
	gitlab.Client // embedded: only the two methods below are exercised

	pipelines []gitlab.Pipeline
	jobs      map[int64][]gitlab.Job
	listCalls int
	jobsCalls int
	listErr   error
}

func (s *stubGitLab) RecentSuccessfulPipelines(_ context.Context, _ int64, limit int) ([]gitlab.Pipeline, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	if limit < len(s.pipelines) {
		return s.pipelines[:limit], nil
	}
	return s.pipelines, nil
}

func (s *stubGitLab) PipelineJobs(_ context.Context, _, pipelineID int64) ([]gitlab.Job, error) {
	s.jobsCalls++
	return s.jobs[pipelineID], nil
}

// fixture builds a stub whose pipelines 1..n each ran for n minutes on ref
// "main", plus one pipeline on "feature-x" that must be excludable.
func fixture() *stubGitLab {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	s := &stubGitLab{jobs: map[int64][]gitlab.Job{}}
	for id := int64(1); id <= 4; id++ {
		ref := "main"
		if id == 4 {
			ref = "feature-x"
		}
		s.pipelines = append([]gitlab.Pipeline{{ID: id, Ref: ref}}, s.pipelines...)
		s.jobs[id] = []gitlab.Job{{
			Stage: "build", Name: "compile",
			StartedAt:  base,
			FinishedAt: base.Add(time.Duration(id) * time.Minute),
		}}
	}
	return s
}

func newFetcher(gl gitlab.Client, ttl time.Duration, now func() time.Time) *Fetcher {
	return &Fetcher{GitLab: gl, Pipelines: 3, TTL: ttl, Log: zap.NewNop(), now: now}
}

func TestFetcherCachesPerProjectAndRef(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s := fixture()
	f := newFetcher(s, time.Hour, func() time.Time { return clock })

	first, err := f.Baseline(t.Context(), 7, []string{"feature-x"})
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if first.Pipeline.Samples != 3 {
		t.Fatalf("samples = %d, want 3 (feature-x excluded)", first.Pipeline.Samples)
	}
	if first.Pipeline.Median != 2*time.Minute {
		t.Errorf("median = %s, want 2m (1m, 2m, 3m)", first.Pipeline.Median)
	}
	calls := s.listCalls

	// Same project + ref within the TTL: served from cache, no API calls.
	if _, err := f.Baseline(t.Context(), 7, []string{"feature-x"}); err != nil {
		t.Fatalf("Baseline (cached): %v", err)
	}
	if s.listCalls != calls {
		t.Errorf("list calls = %d, want %d — second call should hit the cache", s.listCalls, calls)
	}

	// A different ref is a different cache key: it refetches.
	if _, err := f.Baseline(t.Context(), 7, []string{"main"}); err != nil {
		t.Fatalf("Baseline (other ref): %v", err)
	}
	if s.listCalls != calls+1 {
		t.Errorf("list calls = %d, want %d — a new ref must refetch", s.listCalls, calls+1)
	}
}

func TestFetcherRefetchesAfterTTL(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s := fixture()
	f := newFetcher(s, time.Hour, func() time.Time { return clock })

	if _, err := f.Baseline(t.Context(), 7, []string{"feature-x"}); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	clock = clock.Add(61 * time.Minute)
	if _, err := f.Baseline(t.Context(), 7, []string{"feature-x"}); err != nil {
		t.Fatalf("Baseline (expired): %v", err)
	}
	if s.listCalls != 2 {
		t.Errorf("list calls = %d, want 2 — an expired entry must refetch", s.listCalls)
	}
}

func TestFetcherZeroTTLDoesNotCache(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s := fixture()
	f := newFetcher(s, 0, func() time.Time { return clock })

	for range 2 {
		if _, err := f.Baseline(t.Context(), 7, nil); err != nil {
			t.Fatalf("Baseline: %v", err)
		}
	}
	if s.listCalls != 2 {
		t.Errorf("list calls = %d, want 2 — TTL 0 disables caching", s.listCalls)
	}
}

func TestFetcherScansWiderThanTheSampleSize(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s := fixture()
	var gotLimit int
	s.pipelines = s.pipelines[:0] // force an empty result; only the limit matters
	f := newFetcher(&limitRecorder{stubGitLab: s, got: &gotLimit}, time.Hour, func() time.Time { return clock })

	if _, err := f.Baseline(t.Context(), 7, nil); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if gotLimit != 3*scanFactor {
		t.Errorf("scan limit = %d, want %d (Pipelines x scanFactor)", gotLimit, 3*scanFactor)
	}
}

type limitRecorder struct {
	*stubGitLab
	got *int
}

func (l *limitRecorder) RecentSuccessfulPipelines(ctx context.Context, projectID int64, limit int) ([]gitlab.Pipeline, error) {
	*l.got = limit
	return l.stubGitLab.RecentSuccessfulPipelines(ctx, projectID, limit)
}

func TestFetcherPropagatesListError(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s := fixture()
	s.listErr = context.DeadlineExceeded
	f := newFetcher(s, time.Hour, func() time.Time { return clock })

	if _, err := f.Baseline(t.Context(), 7, nil); err == nil {
		t.Fatal("want an error when the pipeline listing fails")
	}
	// A failed fetch must not be cached as an empty baseline.
	s.listErr = nil
	b, err := f.Baseline(t.Context(), 7, nil)
	if err != nil {
		t.Fatalf("Baseline after recovery: %v", err)
	}
	if b.Pipeline.Samples == 0 {
		t.Error("a recovered fetch must produce samples, not a cached empty baseline")
	}
}

func TestCacheEvictsOldestAtCap(t *testing.T) {
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	c := newCache(time.Hour, 2)

	c.put(cacheKey{projectID: 1}, Baseline{}, clock)
	clock = clock.Add(time.Minute)
	c.put(cacheKey{projectID: 2}, Baseline{}, clock)
	clock = clock.Add(time.Minute)
	c.put(cacheKey{projectID: 3}, Baseline{}, clock) // evicts project 1

	if _, ok := c.get(cacheKey{projectID: 1}, clock); ok {
		t.Error("project 1 should have been evicted as the oldest entry")
	}
	for _, id := range []int64{2, 3} {
		if _, ok := c.get(cacheKey{projectID: id}, clock); !ok {
			t.Errorf("project %d should still be cached", id)
		}
	}
}
