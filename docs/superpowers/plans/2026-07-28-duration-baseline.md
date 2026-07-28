# Duration Baseline Comparison Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show in the MR report whether this pipeline — and each of its jobs — is slower than the median of recent successful pipelines on *other* refs, annotating any change beyond ±5%.

**Architecture:** A new `internal/history` package fetches the last N successful pipelines of the project (excluding the reported pipeline's own refs), reduces their job lists to median durations, and caches the reduced result per `{projectID, ref}` for an hour. `reporter.Build` calls it through a `history.Source` interface and copies plain `time.Duration` values onto `report.Data`; `internal/report` renders the delta and gains no new dependency. The feature is switched by `report.compare.enabled` — when false, `Reporter.History` is nil and no extra API call happens.

**Tech Stack:** Go 1.26, `gitlab.com/gitlab-org/api/client-go`, `spf13/viper` + `spf13/cobra` (config), `go.uber.org/zap` (logging), stdlib `testing` with golden files, `helm-unittest` for the chart.

**Spec:** `docs/superpowers/specs/2026-07-28-duration-baseline-design.md`

**Conventions that apply to every task:**
- Run tests with `mise r test` (which is `go test -race ./...`). Single-package runs use `go test -race ./internal/<pkg>/ -run <TestName> -v`.
- Wrap errors with `fmt.Errorf("...: %w", err)`. Typed zap fields only (`zap.Int64`, `zap.String`, `zap.Error`).
- Commit after each task with the message given in the task's final step.

---

## File Structure

**Created:**

| File | Responsibility |
|---|---|
| `internal/history/history.go` | `JobKey`, `Stat`, `Baseline`, `Source` interface, `minSamples` constant, median math |
| `internal/history/history_test.go` | Median/sampling/ref-filter table tests against a stub `gitlab.Client` |
| `internal/history/fetcher.go` | `Fetcher`: GitLab fan-out (scan → filter → reduce) implementing `Source` |
| `internal/history/cache.go` | TTL + capped cache used by `Fetcher` |
| `internal/history/cache_test.go` | Cache hit/miss/expiry/eviction with an injected clock |
| `internal/report/duration.go` | Duration cell + delta formatting (`durationCell`, `deltaSuffix`) |

**Modified:**

| File | Change |
|---|---|
| `internal/gitlab/client.go` | `Pipeline` struct; `RecentSuccessfulPipelines` + `PipelineRef` on `Client` |
| `internal/gitlab/gitlab.go` | REST implementations of both new methods |
| `internal/report/report.go` | New `Data`/`JobReport` fields, `Duration` column, summary delta, footnote |
| `internal/report/report_test.go` | Baseline fields in the golden fixture + new golden cases |
| `internal/report/testdata/report.md` | Regenerated with the `Duration` column |
| `internal/reporter/reporter.go` | `History` field; `Build` takes `excludeRefs`; maps `Baseline` onto `Data` |
| `internal/reporter/reporter_test.go` | Stub `history.Source`, new `Build` signature, stub client methods |
| `internal/config/config.go` | Four `report.compare.*` settings, `Config` fields, validation |
| `internal/config/config_test.go` | Settings-table + validation cases |
| `cmd/bot/deps.go` | Build the `history.Fetcher` when enabled |
| `cmd/bot/run.go` | Resolve the pipeline ref, pass it to `Build` |
| `internal/e2e/e2e_test.go` | Mock pipelines endpoint; assert the delta and ref exclusion |
| `deploy/chart/cigar/values.yaml` | `config.report.compare` block |
| `deploy/chart/cigar/templates/configmap.yaml` | Render the block |
| `deploy/chart/cigar/tests/config_test.yaml` | Assert defaults + an override |
| `docs/usage.md`, `docs/deploy.md`, `README.md` | Document the four knobs; refresh the report sample |

---

## Task 1: GitLab client — recent successful pipelines and a pipeline's ref

**Files:**
- Modify: `internal/gitlab/client.go` (add `Pipeline` type + two `Client` methods)
- Modify: `internal/gitlab/gitlab.go` (implementations)
- Test: `internal/gitlab/gitlab_test.go` (append two tests)

- [x] **Step 1: Write the failing tests**

Append to `internal/gitlab/gitlab_test.go`. Note how the existing tests in this file build a client against an `httptest` server — reuse that shape; if the file has a helper for it, use the helper instead of re-rolling the server.

```go
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
```

Add any missing imports to the test file: `fmt`, `net/http`, `net/http/httptest`, `reflect`, `strings`, `testing`, `go.uber.org/zap`.

- [x] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./internal/gitlab/ -run 'TestRecentSuccessfulPipelines|TestPipelineRef' -v`
Expected: FAIL — compile error, `c.RecentSuccessfulPipelines undefined` and `Pipeline` undefined.

- [x] **Step 3: Extend the Client interface**

In `internal/gitlab/client.go`, add the type just below the `Job` struct:

```go
// Pipeline is a past pipeline of the project, used to build duration baselines.
type Pipeline struct {
	ID  int64
	Ref string
}
```

and these two methods inside the `Client` interface, after `PipelineJobs`:

```go
	// RecentSuccessfulPipelines returns up to limit of the project's most recent
	// successful pipelines, newest first, across all refs.
	RecentSuccessfulPipelines(ctx context.Context, projectID int64, limit int) ([]Pipeline, error)

	// PipelineRef returns the ref a pipeline ran on. Used by `bot run`, which
	// has no webhook payload to read it from.
	PipelineRef(ctx context.Context, projectID, pipelineID int64) (string, error)
```

- [x] **Step 4: Implement both methods**

In `internal/gitlab/gitlab.go`, add after `PipelineJobs`:

```go
func (a *apiClient) RecentSuccessfulPipelines(ctx context.Context, projectID int64, limit int) ([]Pipeline, error) {
	if limit <= 0 {
		return nil, nil
	}
	perPage := min(limit, 100)
	opts := &gl.ListProjectPipelinesOptions{
		Status:      gl.Ptr(gl.Success),
		ListOptions: gl.ListOptions{PerPage: perPage},
	}
	var out []Pipeline
	for {
		page, resp, err := a.c.Pipelines.ListProjectPipelines(projectID, opts, gl.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("list successful pipelines of project %d: %w", projectID, err)
		}
		for _, p := range page {
			out = append(out, Pipeline{ID: int64(p.ID), Ref: p.Ref})
			if len(out) == limit {
				return out, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	a.log.Debug("fetched recent successful pipelines",
		zap.Int64("project_id", projectID), zap.Int("pipelines", len(out)))
	return out, nil
}

func (a *apiClient) PipelineRef(ctx context.Context, projectID, pipelineID int64) (string, error) {
	p, _, err := a.c.Pipelines.GetPipeline(projectID, int(pipelineID), gl.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("get pipeline %d: %w", pipelineID, err)
	}
	return p.Ref, nil
}
```

The exact `client-go` symbols (`gl.Ptr`, `gl.Success`, `ListProjectPipelinesOptions`, `p.ID`'s type, `GetPipeline`'s id parameter type) must be checked against the vendored version — run `go doc gitlab.com/gitlab-org/api/client-go ListProjectPipelinesOptions` and `go doc gitlab.com/gitlab-org/api/client-go PipelinesService.GetPipeline` and adapt. If `PipelineInfo.ID` is already `int64`, drop the conversion. The surrounding file uses `new(x)` as its pointer helper in places — match whatever the package actually exports.

- [x] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/gitlab/ -run 'TestRecentSuccessfulPipelines|TestPipelineRef' -v`
Expected: PASS (3 tests).

- [x] **Step 6: Fix the other Client implementations**

Adding interface methods breaks every stub. Run `go build ./... && go vet ./...` and add stubs where the compiler complains — at minimum `internal/reporter/reporter_test.go`'s `fakeGitLab` and any stub in `internal/command`, `internal/correlate`, `internal/chart`. Use this shape (grouped with the file's existing "unused by this path" stubs):

```go
func (f *fakeGitLab) RecentSuccessfulPipelines(context.Context, int64, int) ([]gitlab.Pipeline, error) {
	return nil, nil
}
func (f *fakeGitLab) PipelineRef(context.Context, int64, int64) (string, error) { return "", nil }
```

- [x] **Step 7: Verify the whole build and suite**

Run: `mise r test`
Expected: PASS, no compile errors.

- [x] **Step 8: Commit**

```bash
git add internal/gitlab cmd internal/reporter internal/command internal/correlate
git commit -m "feat(gitlab): list recent successful pipelines and a pipeline's ref"
```

---

## Task 2: history package — types and median math

**Files:**
- Create: `internal/history/history.go`
- Test: `internal/history/history_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/history/history_test.go`:

```go
package history

import (
	"testing"
	"time"
)

func TestMedian(t *testing.T) {
	tests := []struct {
		name string
		in   []time.Duration
		want time.Duration
		ok   bool
	}{
		{"odd count picks the middle", []time.Duration{
			3 * time.Minute, time.Minute, 2 * time.Minute,
		}, 2 * time.Minute, true},
		{"even count averages the two middles", []time.Duration{
			4 * time.Minute, time.Minute, 3 * time.Minute, 2 * time.Minute,
		}, 150 * time.Second, true},
		{"below minSamples yields nothing", []time.Duration{
			time.Minute, 2 * time.Minute,
		}, 0, false},
		{"empty yields nothing", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := newStat(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if got.Median != tt.want {
				t.Errorf("median = %s, want %s", got.Median, tt.want)
			}
			if got.Samples != len(tt.in) {
				t.Errorf("samples = %d, want %d", got.Samples, len(tt.in))
			}
		})
	}
}

func TestNewStatDoesNotMutateInput(t *testing.T) {
	in := []time.Duration{3 * time.Minute, time.Minute, 2 * time.Minute}
	newStat(in)
	if in[0] != 3*time.Minute {
		t.Errorf("input was sorted in place: %v", in)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race ./internal/history/ -run TestMedian -v`
Expected: FAIL — `newStat` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/history/history.go`:

```go
// Package history answers "what did this normally take": it reduces the
// project's recent successful pipelines to median durations, so a report can
// say whether this pipeline and its jobs are slower than usual. Pipelines on
// the reported refs are excluded by the caller — comparing a branch against
// itself measures iteration noise, not the cost of the change.
package history

import (
	"context"
	"slices"
	"time"
)

// minSamples is how many baseline pipelines a median needs before it is worth
// showing. Below it, no Stat is produced at all.
const minSamples = 3

// JobKey identifies a job across pipelines.
type JobKey struct {
	Stage string
	Name  string
}

// Stat is a median duration and how many pipelines backed it.
type Stat struct {
	Median  time.Duration
	Samples int
}

// Baseline is the typical duration of a pipeline and of each of its jobs. A
// zero Baseline (no samples anywhere) is valid and renders no comparison.
type Baseline struct {
	Pipeline Stat
	Jobs     map[JobKey]Stat
}

// Source is the boundary the reporter depends on; tests stub it.
type Source interface {
	// Baseline returns typical durations from the project's recent successful
	// pipelines, ignoring any pipeline whose ref is in excludeRefs. An empty
	// excludeRefs filters nothing.
	Baseline(ctx context.Context, projectID int64, excludeRefs []string) (Baseline, error)
}

// newStat reduces samples to a median. ok is false below minSamples: a median
// of one or two runs is noise dressed up as a number.
func newStat(samples []time.Duration) (Stat, bool) {
	if len(samples) < minSamples {
		return Stat{}, false
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	median := sorted[mid]
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	}
	return Stat{Median: median, Samples: len(sorted)}, true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/history/ -v`
Expected: PASS (both tests, all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/history
git commit -m "feat(history): baseline types and median reduction"
```

---

## Task 3: history — reduce pipeline job lists to a Baseline

**Files:**
- Create: `internal/history/fetcher.go`
- Modify: `internal/history/history_test.go` (append)

This task builds the pure reduction (job lists → `Baseline`) and its ref filtering. The GitLab fan-out is wired in Task 4.

- [ ] **Step 1: Write the failing test**

Append to `internal/history/history_test.go`:

```go
func TestPipelineWallClock(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	jobs := []gitlab.Job{
		{Stage: "build", Name: "compile", StartedAt: base, FinishedAt: base.Add(2 * time.Minute)},
		{Stage: "test", Name: "unit", StartedAt: base.Add(time.Minute), FinishedAt: base.Add(5 * time.Minute)},
		{Stage: "deploy", Name: "staging"}, // never ran
	}
	got, ok := pipelineWallClock(jobs)
	if !ok {
		t.Fatal("ok = false, want a wall clock")
	}
	if got != 5*time.Minute {
		t.Errorf("wall clock = %s, want 5m (max finish - min start)", got)
	}

	if _, ok := pipelineWallClock([]gitlab.Job{{Stage: "s", Name: "n"}}); ok {
		t.Error("a pipeline with no run window must not produce a wall clock")
	}
}

func TestJobDurationsKeepsLastFinishingRetry(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	jobs := []gitlab.Job{
		// First attempt: 1m. Retry: 3m, finishing later — the retry wins.
		{Stage: "test", Name: "unit", StartedAt: base, FinishedAt: base.Add(time.Minute)},
		{Stage: "test", Name: "unit", StartedAt: base.Add(2 * time.Minute), FinishedAt: base.Add(5 * time.Minute)},
		{Stage: "deploy", Name: "staging"}, // never ran, contributes nothing
	}
	got := jobDurations(jobs)
	if len(got) != 1 {
		t.Fatalf("got %d job keys, want 1: %+v", len(got), got)
	}
	if d := got[JobKey{Stage: "test", Name: "unit"}]; d != 3*time.Minute {
		t.Errorf("duration = %s, want 3m (the last-finishing attempt)", d)
	}
}

func TestSelectSamplesExcludesRefs(t *testing.T) {
	all := []gitlab.Pipeline{
		{ID: 10, Ref: "feature-x"},
		{ID: 9, Ref: "main"},
		{ID: 8, Ref: "refs/merge-requests/3/head"},
		{ID: 7, Ref: "main"},
		{ID: 6, Ref: "other"},
	}
	got := selectSamples(all, []string{"feature-x", "refs/merge-requests/3/head"}, 3)
	want := []int64{9, 7, 6}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want ids %v", got, want)
	}
	for i, p := range got {
		if p.ID != want[i] {
			t.Errorf("sample %d = %d, want %d", i, p.ID, want[i])
		}
	}

	// Newest-first order is preserved and the limit truncates the tail.
	if got := selectSamples(all, nil, 2); len(got) != 2 || got[0].ID != 10 || got[1].ID != 9 {
		t.Errorf("got %+v, want the two newest", got)
	}
	// No exclusions and no limit slack: an empty excludeRefs filters nothing.
	if got := selectSamples(all, []string{}, 10); len(got) != 5 {
		t.Errorf("got %d samples, want all 5", len(got))
	}
}

func TestReduceBuildsMedians(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	// Three pipelines: wall clocks 2m, 4m, 6m -> median 4m.
	// "build : compile" runs in all three (1m, 2m, 3m -> 2m);
	// "test : flaky" runs in only two, so it must produce no Stat.
	mk := func(wall, compile time.Duration, withFlaky bool) []gitlab.Job {
		jobs := []gitlab.Job{
			{Stage: "build", Name: "compile", StartedAt: base, FinishedAt: base.Add(compile)},
			{Stage: "e2e", Name: "run", StartedAt: base, FinishedAt: base.Add(wall)},
		}
		if withFlaky {
			jobs = append(jobs, gitlab.Job{
				Stage: "test", Name: "flaky", StartedAt: base, FinishedAt: base.Add(time.Minute),
			})
		}
		return jobs
	}
	b := reduce([][]gitlab.Job{
		mk(2*time.Minute, time.Minute, true),
		mk(4*time.Minute, 2*time.Minute, false),
		mk(6*time.Minute, 3*time.Minute, true),
	})

	if b.Pipeline.Median != 4*time.Minute || b.Pipeline.Samples != 3 {
		t.Errorf("pipeline stat = %+v, want median 4m over 3 samples", b.Pipeline)
	}
	compile, ok := b.Jobs[JobKey{Stage: "build", Name: "compile"}]
	if !ok || compile.Median != 2*time.Minute || compile.Samples != 3 {
		t.Errorf("compile stat = %+v (ok=%v), want median 2m over 3 samples", compile, ok)
	}
	if _, ok := b.Jobs[JobKey{Stage: "test", Name: "flaky"}]; ok {
		t.Error("a job seen in only 2 pipelines must not get a Stat")
	}
}
```

Add `"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./internal/history/ -v`
Expected: FAIL — `pipelineWallClock`, `jobDurations`, `selectSamples`, `reduce` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/history/fetcher.go`:

```go
package history

import (
	"slices"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
)

// scanFactor widens the pipeline listing: GitLab cannot express "ref != X", so
// refs are filtered client-side and the scan needs slack to still yield a full
// sample set after the reported refs drop out.
const scanFactor = 3

// pipelineWallClock is a sample pipeline's duration under the same definition
// the report prints: max finish - min start across jobs that ran. ok is false
// when no job carries a run window, so such a pipeline is never counted as 0.
func pipelineWallClock(jobs []gitlab.Job) (time.Duration, bool) {
	var start, end time.Time
	for _, j := range jobs {
		if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
			continue
		}
		if start.IsZero() || j.StartedAt.Before(start) {
			start = j.StartedAt
		}
		if j.FinishedAt.After(end) {
			end = j.FinishedAt
		}
	}
	if start.IsZero() || !end.After(start) {
		return 0, false
	}
	return end.Sub(start), true
}

// jobDurations maps each job identity of one pipeline to its duration. A retried
// job appears twice in the listing; the last-finishing attempt wins, since that
// is the one whose duration the pipeline actually paid for.
func jobDurations(jobs []gitlab.Job) map[JobKey]time.Duration {
	out := make(map[JobKey]time.Duration, len(jobs))
	finish := make(map[JobKey]time.Time, len(jobs))
	for _, j := range jobs {
		if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
			continue
		}
		k := JobKey{Stage: j.Stage, Name: j.Name}
		if prev, seen := finish[k]; seen && !j.FinishedAt.After(prev) {
			continue
		}
		finish[k] = j.FinishedAt
		out[k] = j.FinishedAt.Sub(j.StartedAt)
	}
	return out
}

// selectSamples drops pipelines on the excluded refs and keeps the newest limit
// survivors, preserving the newest-first order of the listing.
func selectSamples(all []gitlab.Pipeline, excludeRefs []string, limit int) []gitlab.Pipeline {
	out := make([]gitlab.Pipeline, 0, min(limit, len(all)))
	for _, p := range all {
		if slices.Contains(excludeRefs, p.Ref) {
			continue
		}
		out = append(out, p)
		if len(out) == limit {
			break
		}
	}
	return out
}

// reduce turns the sample pipelines' job listings into medians.
func reduce(pipelines [][]gitlab.Job) Baseline {
	var wall []time.Duration
	perJob := map[JobKey][]time.Duration{}
	for _, jobs := range pipelines {
		if d, ok := pipelineWallClock(jobs); ok {
			wall = append(wall, d)
		}
		for k, d := range jobDurations(jobs) {
			perJob[k] = append(perJob[k], d)
		}
	}
	b := Baseline{Jobs: make(map[JobKey]Stat, len(perJob))}
	b.Pipeline, _ = newStat(wall)
	for k, samples := range perJob {
		if s, ok := newStat(samples); ok {
			b.Jobs[k] = s
		}
	}
	return b
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/history/ -v`
Expected: PASS, all tests including the Task 2 ones.

- [ ] **Step 5: Commit**

```bash
git add internal/history
git commit -m "feat(history): reduce sample pipelines to median durations"
```

---

## Task 4: history — the Fetcher and its cache

**Files:**
- Modify: `internal/history/fetcher.go` (add the `Fetcher` type + `Baseline` method)
- Create: `internal/history/cache.go`
- Create: `internal/history/cache_test.go`

- [ ] **Step 1: Write the failing cache test**

Create `internal/history/cache_test.go`:

```go
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

	pipelines  []gitlab.Pipeline
	jobs       map[int64][]gitlab.Job
	listCalls  int
	jobsCalls  int
	listErr    error
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./internal/history/ -run 'TestFetcher|TestCache' -v`
Expected: FAIL — `Fetcher`, `newCache`, `cacheKey` undefined.

- [ ] **Step 3: Write the cache**

Create `internal/history/cache.go`:

```go
package history

import (
	"sync"
	"time"
)

// defaultCacheCap bounds the cache so a bot watching many projects cannot grow
// without limit. At the cap the oldest entry is evicted.
const defaultCacheCap = 500

// cacheKey is a project plus the primary excluded ref: a baseline computed for
// one branch is not valid for another, because each excludes different samples.
type cacheKey struct {
	projectID int64
	ref       string
}

type cacheEntry struct {
	baseline  Baseline
	fetchedAt time.Time
}

// cache is a TTL map of reduced baselines. Nothing raw is kept — a hit costs no
// GitLab call and no recomputation.
type cache struct {
	mu  sync.Mutex
	ttl time.Duration
	cap int
	m   map[cacheKey]cacheEntry
}

func newCache(ttl time.Duration, capacity int) *cache {
	return &cache{ttl: ttl, cap: capacity, m: map[cacheKey]cacheEntry{}}
}

// get returns the cached baseline when it exists and is younger than the TTL,
// evicting it otherwise. A zero TTL disables caching entirely.
func (c *cache) get(k cacheKey, now time.Time) (Baseline, bool) {
	if c.ttl <= 0 {
		return Baseline{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return Baseline{}, false
	}
	if now.Sub(e.fetchedAt) >= c.ttl {
		delete(c.m, k)
		return Baseline{}, false
	}
	return e.baseline, true
}

func (c *cache) put(k cacheKey, b Baseline, now time.Time) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[k]; !exists && len(c.m) >= c.cap {
		c.evictOldestLocked()
	}
	c.m[k] = cacheEntry{baseline: b, fetchedAt: now}
}

// evictOldestLocked drops the least recently fetched entry. The caller holds mu.
func (c *cache) evictOldestLocked() {
	var (
		oldestKey cacheKey
		oldest    time.Time
		found     bool
	)
	for k, e := range c.m {
		if !found || e.fetchedAt.Before(oldest) {
			oldestKey, oldest, found = k, e.fetchedAt, true
		}
	}
	if found {
		delete(c.m, oldestKey)
	}
}
```

- [ ] **Step 4: Write the Fetcher**

Append to `internal/history/fetcher.go` (and extend its imports with `context`, `fmt`, `sync`, `go.uber.org/zap`):

```go
// Fetcher implements Source against the GitLab API, caching each reduced
// baseline for TTL. Fetches are sequential: this runs in the async worker, the
// cost is hourly per project-branch, and sequential is gentle on GitLab's rate
// limits.
type Fetcher struct {
	GitLab    gitlab.Client
	Pipelines int           // sample size; validated >= minSamples at config load
	TTL       time.Duration // cache entry lifetime; 0 disables caching
	Log       *zap.Logger   // named "history"

	// now is injectable for tests; nil means time.Now.
	now func() time.Time

	once  sync.Once
	cache *cache
}

func (f *Fetcher) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

func (f *Fetcher) init() {
	f.once.Do(func() { f.cache = newCache(f.TTL, defaultCacheCap) })
}

// Baseline implements Source.
func (f *Fetcher) Baseline(ctx context.Context, projectID int64, excludeRefs []string) (Baseline, error) {
	f.init()
	var primary string
	if len(excludeRefs) > 0 {
		primary = excludeRefs[0]
	}
	key := cacheKey{projectID: projectID, ref: primary}
	now := f.clock()
	if b, ok := f.cache.get(key, now); ok {
		f.Log.Debug("baseline cache hit",
			zap.Int64("project_id", projectID), zap.String("ref", primary))
		return b, nil
	}

	all, err := f.GitLab.RecentSuccessfulPipelines(ctx, projectID, f.Pipelines*scanFactor)
	if err != nil {
		return Baseline{}, fmt.Errorf("list baseline pipelines: %w", err)
	}
	samples := selectSamples(all, excludeRefs, f.Pipelines)
	listings := make([][]gitlab.Job, 0, len(samples))
	for _, p := range samples {
		jobs, err := f.GitLab.PipelineJobs(ctx, projectID, p.ID)
		if err != nil {
			return Baseline{}, fmt.Errorf("list jobs of baseline pipeline %d: %w", p.ID, err)
		}
		listings = append(listings, jobs)
	}
	b := reduce(listings)
	f.cache.put(key, b, now)
	f.Log.Debug("baseline computed",
		zap.Int64("project_id", projectID),
		zap.String("ref", primary),
		zap.Int("scanned", len(all)),
		zap.Int("samples", len(samples)),
		zap.Duration("pipeline_median", b.Pipeline.Median))
	return b, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/history/ -v`
Expected: PASS, every test in the package.

- [ ] **Step 6: Lint**

Run: `mise r lint`
Expected: clean. (`golangci-lint` may object to the unused embedded `gitlab.Client` field pattern in the test stub — if so, replace the embedding with explicit no-op stubs for the remaining interface methods, mirroring `internal/reporter/reporter_test.go`'s `fakeGitLab`.)

- [ ] **Step 7: Commit**

```bash
git add internal/history
git commit -m "feat(history): TTL-cached baseline fetcher over the GitLab API"
```

---

## Task 5: report — duration column, deltas and footnote

**Files:**
- Create: `internal/report/duration.go`
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`
- Modify: `internal/report/testdata/report.md` (regenerated)

- [ ] **Step 1: Write the failing formatting tests**

Append to `internal/report/report_test.go`:

```go
func TestDurationCell(t *testing.T) {
	const ratio = 0.05
	tests := []struct {
		name     string
		current  time.Duration
		baseline time.Duration
		samples  int
		want     string
	}{
		{"no baseline prints the duration alone",
			4 * time.Minute, 0, 0, "4m 00s"},
		{"below minimum samples prints no delta",
			4 * time.Minute, 2 * time.Minute, 2, "4m 00s"},
		{"within the threshold prints no delta",
			// 4m02s vs 4m median = +0.8%, under 5%.
			4*time.Minute + 2*time.Second, 4 * time.Minute, 6, "4m 02s"},
		{"slower beyond the threshold",
			// 6m20s vs 4m12s = +2m08s, +50.79% -> +51%.
			6*time.Minute + 20*time.Second, 4*time.Minute + 12*time.Second, 6,
			"6m 20s 🔺 +2m 08s (+51%)"},
		{"faster beyond the threshold",
			40 * time.Second, 55 * time.Second, 6, "40s 🔻 −15s (−27%)"},
		{"zero current duration prints an em dash",
			0, 4 * time.Minute, 6, "—"},
		{"zero baseline never divides",
			4 * time.Minute, 0, 6, "4m 00s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := durationCell(tt.current, tt.baseline, tt.samples, ratio)
			if got != tt.want {
				t.Errorf("durationCell = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDurationCellZeroRatioShowsEveryChange(t *testing.T) {
	// A 0 threshold means "annotate any measurable change".
	got := durationCell(4*time.Minute+1*time.Second, 4*time.Minute, 6, 0)
	if !strings.Contains(got, "🔺") {
		t.Errorf("durationCell = %q, want a slower marker", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./internal/report/ -run TestDurationCell -v`
Expected: FAIL — `durationCell` undefined.

- [ ] **Step 3: Write the formatter**

Create `internal/report/duration.go`:

```go
package report

import (
	"fmt"
	"math"
	"time"
)

// minBaselineSamples mirrors history's minimum: a median backed by fewer runs
// is noise, so its delta is not rendered even if the data reaches us.
const minBaselineSamples = 3

// durationCell renders a duration, followed by a delta against baseline when the
// comparison is trustworthy (enough samples) and material (beyond warnRatio).
// A zero current duration means the job never ran — never rendered as 0s.
func durationCell(current, baseline time.Duration, samples int, warnRatio float64) string {
	if current <= 0 {
		return dash
	}
	cell := humanDuration(current)
	if suffix := deltaSuffix(current, baseline, samples, warnRatio); suffix != "" {
		cell += " " + suffix
	}
	return cell
}

// deltaSuffix is the "🔺 +2m 08s (+51%)" part, or empty when there is no
// trustworthy or material change to report.
func deltaSuffix(current, baseline time.Duration, samples int, warnRatio float64) string {
	if samples < minBaselineSamples || baseline <= 0 || current <= 0 {
		return ""
	}
	delta := current - baseline
	ratio := float64(delta) / float64(baseline)
	if math.Abs(ratio) <= warnRatio {
		return ""
	}
	marker, sign := "🔺", "+"
	if delta < 0 {
		marker, sign = "🔻", "−" // U+2212 minus, not a hyphen
		delta = -delta
	}
	return fmt.Sprintf("%s %s%s (%s%.0f%%)",
		marker, sign, humanDuration(delta), sign, math.Abs(ratio)*100)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/report/ -run TestDurationCell -v`
Expected: PASS (both tests, all subtests).

- [ ] **Step 5: Commit the formatter**

```bash
git add internal/report/duration.go internal/report/report_test.go
git commit -m "feat(report): format duration deltas against a baseline"
```

- [ ] **Step 6: Write the failing rendering test**

Append to `internal/report/report_test.go`:

```go
func TestRenderDurationComparison(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	// Wall clock 6m20s against a 4m12s median: +2m08s (+51%).
	d := Data{
		PipelineID:               99,
		Status:                   "success",
		ThrottleWarnRatio:        0.25,
		DurationDeltaRatio:       0.05,
		BaselinePipelineDuration: 4*time.Minute + 12*time.Second,
		BaselinePipelineSamples:  6,
		Jobs: []JobReport{
			{Stage: "build", Name: "compile",
				StartedAt:        base,
				FinishedAt:       base.Add(6*time.Minute + 20*time.Second),
				BaselineDuration: 5 * time.Minute,
				BaselineSamples:  6,
				Usage:            &metrics.JobUsage{CPUSeconds: 1}},
		},
	}
	got := mustRender(t, d)

	if !strings.Contains(got, "| Pipeline duration | 6m 20s 🔺 +2m 08s (+51%) |") {
		t.Errorf("summary is missing the pipeline delta:\n%s", got)
	}
	if !strings.Contains(got, "| Duration |") {
		t.Errorf("details table is missing the Duration column:\n%s", got)
	}
	// 6m20s vs a 5m median = +1m20s (+27%).
	if !strings.Contains(got, "6m 20s 🔺 +1m 20s (+27%)") {
		t.Errorf("job row is missing its delta:\n%s", got)
	}
	// A 6-sample baseline at the default size is not thin: no footnote.
	if strings.Contains(got, "Duration deltas vs") {
		t.Errorf("a full baseline must not print the thin-baseline footnote:\n%s", got)
	}
}

func TestRenderThinBaselineFootnote(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	d := Data{
		PipelineID:               99,
		Status:                   "success",
		DurationDeltaRatio:       0.05,
		BaselinePipelineDuration: 2 * time.Minute,
		BaselinePipelineSamples:  4,
		Jobs: []JobReport{
			{Stage: "build", Name: "compile",
				StartedAt:  base,
				FinishedAt: base.Add(4 * time.Minute),
				Usage:      &metrics.JobUsage{CPUSeconds: 1}},
		},
	}
	got := mustRender(t, d)
	want := "_Duration deltas vs the median of 4 recent successful pipelines on other refs._"
	if !strings.Contains(got, want) {
		t.Errorf("missing thin-baseline footnote %q:\n%s", want, got)
	}
}

func TestRenderWithoutBaselineHasDurationsButNoDeltas(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	d := Data{
		PipelineID:         99,
		Status:             "success",
		DurationDeltaRatio: 0.05,
		Jobs: []JobReport{
			{Stage: "build", Name: "compile",
				StartedAt:  base,
				FinishedAt: base.Add(90 * time.Second),
				Usage:      &metrics.JobUsage{CPUSeconds: 1}},
		},
	}
	got := mustRender(t, d)
	if !strings.Contains(got, "1m 30s") {
		t.Errorf("job duration is missing:\n%s", got)
	}
	for _, marker := range []string{"🔺", "🔻", "Duration deltas vs"} {
		if strings.Contains(got, marker) {
			t.Errorf("unexpected %q with no baseline:\n%s", marker, got)
		}
	}
}
```

- [ ] **Step 7: Run the tests to verify they fail**

Run: `go test -race ./internal/report/ -run TestRender -v`
Expected: FAIL — unknown fields `DurationDeltaRatio`, `BaselinePipelineDuration`, `BaselinePipelineSamples`, `BaselineDuration`, `BaselineSamples`.

- [ ] **Step 8: Add the Data/JobReport fields**

In `internal/report/report.go`, extend `JobReport` (after `FinishedAt`):

```go
	// BaselineDuration is this job's typical duration in recent successful
	// pipelines on other refs; 0 means no comparable baseline. BaselineSamples
	// is how many pipelines backed it — below minBaselineSamples no delta is
	// rendered.
	BaselineDuration time.Duration
	BaselineSamples  int
```

and `Data` (after `RanJobs`):

```go
	// BaselinePipelineDuration/Samples are the same comparison for the pipeline
	// as a whole (0 = no baseline). DurationDeltaRatio is the relative change
	// above which a delta is shown at all.
	BaselinePipelineDuration time.Duration
	BaselinePipelineSamples  int
	DurationDeltaRatio       float64
```

- [ ] **Step 9: Add a job-duration accessor**

Add to `internal/report/report.go`, next to `JobReport`:

```go
// duration is the job's run time, or 0 when it never ran.
func (j JobReport) duration() time.Duration {
	if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
		return 0
	}
	return j.FinishedAt.Sub(j.StartedAt)
}
```

- [ ] **Step 10: Render the summary delta, the column and the footnote**

In `Render`, replace the pipeline-duration line:

```go
	if dur, ok := d.wallClock(); ok {
		fmt.Fprintf(&b, "| Pipeline duration | %s |\n",
			durationCell(dur, d.BaselinePipelineDuration, d.BaselinePipelineSamples, d.DurationDeltaRatio))
	}
```

Replace the details header with the `Duration` column second:

```go
	b.WriteString("\n### Details\n\n")
	b.WriteString("| Stage : Job | Duration | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX | Disk R / W |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, j := range d.Jobs {
		row(&b, j, d.ThrottleWarnRatio, d.DurationDeltaRatio)
	}
	if note := d.baselineFootnote(); note != "" {
		b.WriteString("\n" + note + "\n")
	}
```

Update `row` to take the ratio and emit the new cell (the no-usage branch grows one column):

```go
func row(b *strings.Builder, j JobReport, warnRatio, deltaRatio float64) {
	name := fmt.Sprintf("%s : %s", j.Stage, j.Name)
	dur := durationCell(j.duration(), j.BaselineDuration, j.BaselineSamples, deltaRatio)
	if j.Usage == nil {
		fmt.Fprintf(b, "| %s | %s | _no data_ | | | | | | |\n", name, dur)
		return
	}
	u := j.Usage
	fmt.Fprintf(b, "| %s | %s | %s | %s | %s / %s | %s / %s | %s | %s / %s | %s / %s |\n",
		name,
		dur,
		cpuTime(u.CPUSeconds),
		humanBytes(u.PeakMemoryBytes),
		optBytes(u.MemoryRequestBytes), optBytes(u.MemoryLimitBytes),
		cores(u.CPURequestCores), cores(u.CPULimitCores),
		throttle(u.ThrottledRatio, warnRatio),
		humanBytes(u.NetworkRxBytes), humanBytes(u.NetworkTxBytes),
		optBytes(u.DiskReadBytes), optBytes(u.DiskWriteBytes),
	)
}
```

And add the footnote helper next to `hasUsage`:

```go
// baselineFootnote names the sample count when the baseline is thin (fewer than
// fullBaselineSamples), so a median backed by a handful of runs is not
// over-trusted. A full or absent baseline gets no footnote.
func (d Data) baselineFootnote() string {
	n := d.BaselinePipelineSamples
	if n < minBaselineSamples || n >= fullBaselineSamples {
		return ""
	}
	return fmt.Sprintf("_Duration deltas vs the median of %d recent successful pipelines on other refs._", n)
}
```

Add to `internal/report/duration.go`:

```go
// fullBaselineSamples is the sample count at or above which the baseline is
// considered solid enough to need no footnote. It matches the default
// report.compare.history_pipelines.
const fullBaselineSamples = 6
```

- [ ] **Step 11: Run the rendering tests**

Run: `go test -race ./internal/report/ -run TestRender -v`
Expected: `TestRenderDurationComparison`, `TestRenderThinBaselineFootnote`, `TestRenderWithoutBaselineHasDurationsButNoDeltas` PASS; `TestRenderGolden` FAILS because the golden file predates the column.

- [ ] **Step 12: Give the golden fixture a baseline, then regenerate**

In `TestRenderGolden`'s `Data` literal add, after `ThrottleWarnRatio: 0.25,`:

```go
		DurationDeltaRatio:       0.05,
		BaselinePipelineDuration: 3 * time.Minute,
		BaselinePipelineSamples:  6,
```

and give the two jobs that ran a baseline — `compile` gets a faster-than-usual delta, `unit` stays inside the threshold. In the `compile` job literal add:

```go
					BaselineDuration: 4 * time.Minute,
					BaselineSamples:  6,
```

and in the `unit` job literal:

```go
					BaselineDuration: 148 * time.Second,
					BaselineSamples:  6,
```

Then regenerate and inspect:

```bash
go test ./internal/report/ -run TestRenderGolden -update
git diff internal/report/testdata/report.md
```

Expected in the diff: `| Pipeline duration | 4m 12s 🔺 +1m 12s (+40%) |`; a `Duration` column as the second column of the details table; `compile` showing `2m 30s 🔻 −1m 30s (−38%)`; `unit` showing `2m 30s` with **no** delta (150s vs a 148s median is +1.4%); `deploy : staging` showing `—`. No footnote (6 samples is not thin).

- [ ] **Step 13: Run the full package suite**

Run: `go test -race ./internal/report/ -v`
Expected: PASS, all tests.

- [ ] **Step 14: Commit**

```bash
git add internal/report
git commit -m "feat(report): add a duration column with baseline deltas"
```

---

## Task 6: reporter — fetch the baseline and map it onto the report

**Files:**
- Modify: `internal/reporter/reporter.go`
- Test: `internal/reporter/reporter_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/reporter/reporter_test.go`:

```go
type fakeHistory struct {
	baseline    history.Baseline
	err         error
	gotProject  int64
	gotExcludes []string
	calls       int
}

func (f *fakeHistory) Baseline(_ context.Context, projectID int64, excludeRefs []string) (history.Baseline, error) {
	f.calls++
	f.gotProject = projectID
	f.gotExcludes = excludeRefs
	return f.baseline, f.err
}

func TestBuildAppliesBaseline(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	gl := &fakeGitLab{jobs: []gitlab.Job{
		{ID: 1, Stage: "build", Name: "compile", StartedAt: base, FinishedAt: base.Add(3 * time.Minute)},
		{ID: 2, Stage: "test", Name: "new-job", StartedAt: base, FinishedAt: base.Add(time.Minute)},
	}}
	hist := &fakeHistory{baseline: history.Baseline{
		Pipeline: history.Stat{Median: 2 * time.Minute, Samples: 5},
		Jobs: map[history.JobKey]history.Stat{
			{Stage: "build", Name: "compile"}: {Median: 90 * time.Second, Samples: 5},
		},
	}}
	r := &Reporter{
		GitLab:             gl,
		Resolver:           &fakeResolver{},
		Metrics:            &fakeSource{},
		History:            hist,
		DurationDeltaRatio: 0.05,
		Log:                zap.NewNop(),
	}

	data, err := r.Build(t.Context(), 7, 42, []string{"feature-x", "refs/merge-requests/3/head"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if data.BaselinePipelineDuration != 2*time.Minute || data.BaselinePipelineSamples != 5 {
		t.Errorf("pipeline baseline = %s/%d, want 2m/5",
			data.BaselinePipelineDuration, data.BaselinePipelineSamples)
	}
	if data.DurationDeltaRatio != 0.05 {
		t.Errorf("DurationDeltaRatio = %v, want 0.05", data.DurationDeltaRatio)
	}
	if got := data.Jobs[0].BaselineDuration; got != 90*time.Second {
		t.Errorf("compile baseline = %s, want 1m30s", got)
	}
	if got := data.Jobs[1].BaselineSamples; got != 0 {
		t.Errorf("a job absent from the baseline must have 0 samples, got %d", got)
	}
	if want := []string{"feature-x", "refs/merge-requests/3/head"}; !reflect.DeepEqual(hist.gotExcludes, want) {
		t.Errorf("excludeRefs = %v, want %v", hist.gotExcludes, want)
	}
}

func TestBuildSurvivesBaselineFailure(t *testing.T) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	gl := &fakeGitLab{jobs: []gitlab.Job{
		{ID: 1, Stage: "build", Name: "compile", StartedAt: base, FinishedAt: base.Add(time.Minute)},
	}}
	r := &Reporter{
		GitLab:   gl,
		Resolver: &fakeResolver{},
		Metrics:  &fakeSource{},
		History:  &fakeHistory{err: errors.New("gitlab down")},
		Log:      zap.NewNop(),
	}

	data, err := r.Build(t.Context(), 7, 42, []string{"feature-x"})
	if err != nil {
		t.Fatalf("Build must not fail when the baseline fails: %v", err)
	}
	if len(data.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(data.Jobs))
	}
	if data.BaselinePipelineSamples != 0 || data.Jobs[0].BaselineSamples != 0 {
		t.Error("a failed baseline must leave every baseline field zero")
	}
}

func TestBuildWithoutHistorySourceSkipsComparison(t *testing.T) {
	gl := &fakeGitLab{jobs: []gitlab.Job{{ID: 1, Stage: "build", Name: "compile"}}}
	r := &Reporter{GitLab: gl, Resolver: &fakeResolver{}, Metrics: &fakeSource{}, Log: zap.NewNop()}

	data, err := r.Build(t.Context(), 7, 42, []string{"feature-x"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if data.BaselinePipelineSamples != 0 {
		t.Error("a nil History must produce no baseline")
	}
}

func TestProcessPipelineExcludesBothRefForms(t *testing.T) {
	gl := &fakeGitLab{jobs: []gitlab.Job{{ID: 1, Stage: "build", Name: "compile"}}}
	hist := &fakeHistory{}
	r := &Reporter{
		GitLab: gl, Resolver: &fakeResolver{}, Metrics: &fakeSource{},
		History: hist, Log: zap.NewNop(),
	}

	if _, err := r.ProcessPipeline(t.Context(), 7, 42, 3, "feature-x", "success"); err != nil {
		t.Fatalf("ProcessPipeline: %v", err)
	}
	want := []string{"feature-x", "refs/merge-requests/3/head"}
	if !reflect.DeepEqual(hist.gotExcludes, want) {
		t.Errorf("excludeRefs = %v, want %v", hist.gotExcludes, want)
	}
}
```

Add `reflect` and the `history` package to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./internal/reporter/ -v`
Expected: FAIL — unknown fields `History`, `DurationDeltaRatio`, and `Build` takes 3 args not 4.

- [ ] **Step 3: Extend the Reporter struct**

In `internal/reporter/reporter.go`, add to the imports `"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/history"` and to the struct:

```go
	// History supplies typical durations for the delta comparison. Nil disables
	// the comparison entirely (report.compare.enabled=false) — no API calls.
	History history.Source

	// DurationDeltaRatio is the relative duration change above which the report
	// annotates a delta.
	DurationDeltaRatio float64
```

- [ ] **Step 4: Take excludeRefs in Build and apply the baseline**

Change the signature and body in `internal/reporter/reporter.go`:

```go
// Build assembles the report data for one pipeline. Per-job failures (no pod
// correlated, metrics query failed) leave that job's Usage nil rather than
// failing the whole pipeline. excludeRefs are the refs of the pipeline being
// reported: baseline samples on those refs are ignored, so the comparison is
// against other code, not this branch's own earlier runs.
func (r *Reporter) Build(ctx context.Context, projectID, pipelineID int64, excludeRefs []string) (report.Data, error) {
	jobs, err := r.GitLab.PipelineJobs(ctx, projectID, pipelineID)
	if err != nil {
		return report.Data{}, fmt.Errorf("list pipeline jobs: %w", err)
	}
	r.Log.Debug("listed pipeline jobs",
		zap.Int64("project_id", projectID), zap.Int64("pipeline_id", pipelineID), zap.Int("jobs", len(jobs)))

	base := r.baseline(ctx, projectID, excludeRefs)

	data := report.Data{
		PipelineID:               pipelineID,
		ThrottleWarnRatio:        r.ThrottleWarnRatio,
		DurationDeltaRatio:       r.DurationDeltaRatio,
		BaselinePipelineDuration: base.Pipeline.Median,
		BaselinePipelineSamples:  base.Pipeline.Samples,
	}
	for _, job := range jobs {
		if !job.StartedAt.IsZero() && !job.FinishedAt.IsZero() {
			data.RanJobs++
		}
		stat := base.Jobs[history.JobKey{Stage: job.Stage, Name: job.Name}]
		data.Jobs = append(data.Jobs, report.JobReport{
			Stage:            job.Stage,
			Name:             job.Name,
			StartedAt:        job.StartedAt,
			FinishedAt:       job.FinishedAt,
			BaselineDuration: stat.Median,
			BaselineSamples:  stat.Samples,
			Usage:            r.jobUsage(ctx, projectID, job),
		})
	}
	return data, nil
}

// baseline fetches typical durations, degrading to no comparison. A baseline is
// a nicety: losing it must never cost the report, so the error is logged and
// swallowed (only the jobs listing failing aborts a report).
func (r *Reporter) baseline(ctx context.Context, projectID int64, excludeRefs []string) history.Baseline {
	if r.History == nil {
		return history.Baseline{}
	}
	b, err := r.History.Baseline(ctx, projectID, excludeRefs)
	if err != nil {
		r.Log.Warn("duration baseline unavailable, reporting without comparison",
			zap.Int64("project_id", projectID), zap.Strings("exclude_refs", excludeRefs), zap.Error(err))
		return history.Baseline{}
	}
	return b
}
```

- [ ] **Step 5: Pass both ref forms from ProcessPipeline**

In `ProcessPipeline`, replace the `r.Build(ctx, projectID, pipelineID)` call with:

```go
	data, err := r.Build(ctx, projectID, pipelineID, pipelineRefs(ref, mrIID))
```

and add at the bottom of the file (named `pipelineRefs`, not `excludeRefs` — the
latter is a parameter name in `Build` and `baseline`, and a package-level
function of the same name would be shadowed there and read as a bug):

```go
// pipelineRefs is the set of refs that belong to the pipeline being reported.
// The webhook carries the source branch, while the pipelines API reports MR
// pipelines under refs/merge-requests/<iid>/head — both must drop out of the
// baseline, or the branch ends up compared against itself.
func pipelineRefs(ref string, mrIID int64) []string {
	out := make([]string, 0, 2)
	if ref != "" {
		out = append(out, ref)
	}
	if mrIID > 0 {
		out = append(out, fmt.Sprintf("refs/merge-requests/%d/head", mrIID))
	}
	return out
}
```

Note the ordering: `ref` first, because `history.Fetcher` uses the first entry as its cache key.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -race ./internal/reporter/ -v`
Expected: PASS. Pre-existing `Build` calls in the test file need their new fourth argument — pass `nil` where the test does not care about refs.

- [ ] **Step 7: Commit**

```bash
git add internal/reporter
git commit -m "feat(reporter): compare pipeline and job durations to a baseline"
```

---

## Task 7: config — the report.compare block

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (match the file's existing helpers — it already has `newTestRoot`/`writeConfig` and a settings-table test listing every key; extend that list rather than duplicating it):

```go
func TestLoadCompareDefaults(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("PROMETHEUS_URL", "http://prom:9090")

	cfg, err := Load(New())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.CompareEnabled {
		t.Error("CompareEnabled should default to true")
	}
	if cfg.CompareDurationDeltaRatio != 0.05 {
		t.Errorf("CompareDurationDeltaRatio = %v, want 0.05", cfg.CompareDurationDeltaRatio)
	}
	if cfg.CompareHistoryPipelines != 6 {
		t.Errorf("CompareHistoryPipelines = %d, want 6", cfg.CompareHistoryPipelines)
	}
	if cfg.CompareCacheTTL != time.Hour {
		t.Errorf("CompareCacheTTL = %s, want 1h", cfg.CompareCacheTTL)
	}
}

func TestLoadCompareValidation(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("PROMETHEUS_URL", "http://prom:9090")

	t.Run("too few baseline pipelines is rejected", func(t *testing.T) {
		t.Setenv("REPORT_COMPARE_HISTORY_PIPELINES", "2")
		if _, err := Load(New()); err == nil {
			t.Fatal("want an error for a baseline smaller than 3")
		}
	})

	t.Run("the same value is fine when comparison is off", func(t *testing.T) {
		t.Setenv("REPORT_COMPARE_HISTORY_PIPELINES", "2")
		t.Setenv("REPORT_COMPARE_ENABLED", "false")
		cfg, err := Load(New())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.CompareEnabled {
			t.Error("CompareEnabled should be false")
		}
	})

	t.Run("a zero cache TTL is accepted", func(t *testing.T) {
		t.Setenv("REPORT_COMPARE_CACHE_TTL", "0s")
		cfg, err := Load(New())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.CompareCacheTTL != 0 {
			t.Errorf("CompareCacheTTL = %s, want 0", cfg.CompareCacheTTL)
		}
	})

	t.Run("a non-numeric pipeline count is rejected", func(t *testing.T) {
		t.Setenv("REPORT_COMPARE_HISTORY_PIPELINES", "many")
		if _, err := Load(New()); err == nil {
			t.Fatal("want an error for a non-numeric pipeline count")
		}
	})
}
```

Also extend the existing key lists in `config_test.go` (around the `{"report.throttle_warn_ratio", "REPORT_THROTTLE_WARN_RATIO", "report-throttle-warn-ratio"}` case and the all-keys slice) with the four new keys:

```go
		{"report.compare.enabled", "REPORT_COMPARE_ENABLED", "report-compare-enabled"},
		{"report.compare.duration_delta_ratio", "REPORT_COMPARE_DURATION_DELTA_RATIO", "report-compare-duration-delta-ratio"},
		{"report.compare.history_pipelines", "REPORT_COMPARE_HISTORY_PIPELINES", "report-compare-history-pipelines"},
		{"report.compare.cache_ttl", "REPORT_COMPARE_CACHE_TTL", "report-compare-cache-ttl"},
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./internal/config/ -v`
Expected: FAIL — unknown fields `CompareEnabled` etc., and the settings-table test reports the four keys missing.

- [ ] **Step 3: Add the settings**

In `internal/config/config.go`, add to the `settings` slice after the existing `report.*` entries:

```go
	{"report.compare.enabled", "true", "Compare pipeline/job durations against recent successful pipelines"},
	{"report.compare.duration_delta_ratio", "0.05", "Relative duration change above which the report annotates a delta"},
	{"report.compare.history_pipelines", "6", "How many recent successful pipelines form the duration baseline (minimum 3)"},
	{"report.compare.cache_ttl", "1h", "How long a computed duration baseline is cached; 0 disables caching"},
```

- [ ] **Step 4: Add the Config fields**

In the `Config` struct:

```go
	CompareEnabled            bool
	CompareDurationDeltaRatio float64
	CompareHistoryPipelines   int
	CompareCacheTTL           time.Duration
```

- [ ] **Step 5: Parse and validate in Load**

In `Load`, after the `CommandsEnabled` block:

```go
	if cfg.CompareEnabled, err = parseBool(v.GetString("report.compare.enabled"), "REPORT_COMPARE_ENABLED"); err != nil {
		return nil, err
	}
	if cfg.CompareDurationDeltaRatio, err = parseRatio(v.GetString("report.compare.duration_delta_ratio"), "REPORT_COMPARE_DURATION_DELTA_RATIO", true); err != nil {
		return nil, err
	}
	if cfg.CompareHistoryPipelines, err = parseCount(v.GetString("report.compare.history_pipelines"), "REPORT_COMPARE_HISTORY_PIPELINES"); err != nil {
		return nil, err
	}
	if cfg.CompareCacheTTL, err = parseNonNegativeDuration(v.GetString("report.compare.cache_ttl"), "REPORT_COMPARE_CACHE_TTL"); err != nil {
		return nil, err
	}
```

and with the other validations (after the chart-format check):

```go
	// A baseline smaller than the minimum sample count could never produce a
	// comparison, so reject it rather than silently rendering nothing.
	if cfg.CompareEnabled && cfg.CompareHistoryPipelines < minBaselinePipelines {
		return nil, fmt.Errorf("REPORT_COMPARE_HISTORY_PIPELINES must be at least %d, got %d",
			minBaselinePipelines, cfg.CompareHistoryPipelines)
	}
```

Add the helpers next to `parseDuration`:

```go
// minBaselinePipelines mirrors history's minimum sample count.
const minBaselinePipelines = 3

func parseCount(raw, label string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", label, raw)
	}
	return n, nil
}

// parseNonNegativeDuration accepts 0 (used to disable a timer), unlike
// parseDuration which requires a positive value.
func parseNonNegativeDuration(raw, label string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration, got %q", label, raw)
	}
	return d, nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -race ./internal/config/ -v`
Expected: PASS, all tests.

- [ ] **Step 7: Commit**

```bash
git add internal/config
git commit -m "feat(config): add the report.compare settings group"
```

---

## Task 8: wiring — deps.go and `bot run`

**Files:**
- Modify: `cmd/bot/deps.go`
- Modify: `cmd/bot/run.go`

- [ ] **Step 1: Build the Fetcher in newReporter**

In `cmd/bot/deps.go`, import `"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/history"` and, inside `newReporter` just before the `return`:

```go
	// A disabled comparison leaves History nil: the reporter then makes no
	// baseline API calls at all.
	var hist history.Source
	if cfg.CompareEnabled {
		hist = &history.Fetcher{
			GitLab:    gl,
			Pipelines: cfg.CompareHistoryPipelines,
			TTL:       cfg.CompareCacheTTL,
			Log:       log.Named("history"),
		}
	}
```

and extend the returned struct:

```go
	return &reporter.Reporter{
		GitLab:             gl,
		Resolver:           resolver,
		Metrics:            source,
		History:            hist,
		ThrottleWarnRatio:  cfg.ThrottleWarnRatio,
		DurationDeltaRatio: cfg.CompareDurationDeltaRatio,
		SigningKey:         []byte(cfg.CommandsSigningKey),
		Log:                log.Named("reporter"),
	}, nil
```

`history` must be a **new** logger name so its warnings land under `name=history` in `cigar_log_total` rather than in the reporter's bucket.

- [ ] **Step 2: Resolve the ref in `bot run`**

In `cmd/bot/run.go`, replace the `rep.Build(...)` call site:

```go
			// `bot run` has no webhook payload, so ask GitLab which ref this
			// pipeline ran on — the baseline must not include the branch itself.
			var excludeRefs []string
			if cfg.CompareEnabled {
				ref, err := rep.GitLab.PipelineRef(cmd.Context(), projectID, pipelineID)
				if err != nil {
					log.Warn("could not resolve the pipeline ref; the duration baseline may include this branch",
						zap.Int64("pipeline_id", pipelineID), zap.Error(err))
				} else {
					excludeRefs = []string{ref}
				}
			}
			data, err := rep.Build(cmd.Context(), projectID, pipelineID, excludeRefs)
			if err != nil {
				return err
			}
```

- [ ] **Step 3: Add the delta ratio to the single-job path**

In `printJobDetails`, the one-job `report.Data` literal has no baseline (a single job's comparison is the report's job, not this debug view), but it must still carry the ratio so the shared renderer behaves consistently:

```go
		data := report.Data{
			PipelineID:         pipelineID,
			Status:             "job: " + j.Name,
			Jobs:               []report.JobReport{{Stage: j.Stage, Name: j.Name, Usage: usage}},
			ThrottleWarnRatio:  cfg.ThrottleWarnRatio,
			DurationDeltaRatio: cfg.CompareDurationDeltaRatio,
			RanJobs:            1,
		}
```

- [ ] **Step 4: Build and run everything**

Run: `mise r build && mise r test`
Expected: build succeeds, all tests PASS.

- [ ] **Step 5: Verify the binary's flags exist**

Run: `./bot serve --help | grep report-compare`
Expected: four lines — `--report-compare-enabled`, `--report-compare-duration-delta-ratio`, `--report-compare-history-pipelines`, `--report-compare-cache-ttl`. (Adjust the binary path to whatever `mise r build` produced.)

- [ ] **Step 6: Commit**

```bash
git add cmd/bot
git commit -m "feat(cmd): wire the duration baseline into serve and run"
```

---

## Task 9: e2e — the baseline through the whole chain

**Files:**
- Modify: `internal/e2e/e2e_test.go`

- [ ] **Step 1: Serve the baseline endpoints from the mock**

In `mockGitLab.server`, add before the catch-all `"/"` handler. Note `branchRef` is `"feature-x"` and `mrIID` already exist as constants in this file:

```go
	// Baseline: three successful pipelines on other refs (2m, 4m, 6m -> median
	// 4m) plus one on the reported branch and one under the MR ref form, both of
	// which must be excluded from the comparison.
	baselineJobs := map[int64]time.Duration{
		901: 2 * time.Minute,
		902: 4 * time.Minute,
		903: 6 * time.Minute,
		904: 30 * time.Minute, // on branchRef: excluded
		905: 40 * time.Minute, // on refs/merge-requests/<mrIID>/head: excluded
	}
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/pipelines", projectID),
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `[
				{"id":905,"ref":"refs/merge-requests/%d/head","status":"success"},
				{"id":904,"ref":%q,"status":"success"},
				{"id":903,"ref":"main","status":"success"},
				{"id":902,"ref":"main","status":"success"},
				{"id":901,"ref":"main","status":"success"}
			]`, mrIID, branchRef)
		})
	for id, dur := range baselineJobs {
		id, dur := id, dur
		mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/pipelines/%d/jobs", projectID, id),
			func(w http.ResponseWriter, _ *http.Request) {
				start := time.Now().Add(-2 * time.Hour).UTC()
				_, _ = fmt.Fprintf(w, `[{"id":%d,"name":"build","status":"success","started_at":%q,"finished_at":%q}]`,
					id*10, start.Format(time.RFC3339), start.Add(dur).Format(time.RFC3339))
			})
	}
```

- [ ] **Step 2: Wire the Fetcher into the harness**

In `harness`, extend the `reporter.Reporter` literal:

```go
	rep := &reporter.Reporter{
		GitLab:   glClient,
		Resolver: resolver,
		Metrics:  source,
		History: &history.Fetcher{
			GitLab:    glClient,
			Pipelines: 3,
			TTL:       time.Hour,
			Log:       log,
		},
		ThrottleWarnRatio:  0.25,
		DurationDeltaRatio: 0.05,
		Log:                log,
	}
```

Add `"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/history"` to the imports.

- [ ] **Step 3: Write the failing assertion test**

Add a new test to `internal/e2e/e2e_test.go`, using this file's existing `postWebhook` + `waitFor` helpers (the same shape as `TestWebhookToMRNote`):

```go
// TestPipelineReportComparesDuration proves the whole chain produces a duration
// delta, and that pipelines on the reported refs are excluded from the median.
func TestPipelineReportComparesDuration(t *testing.T) {
	app, glMock, _ := harness(t, "trace")

	payload := fmt.Sprintf(`{
		"object_kind": "pipeline",
		"object_attributes": {"id": %d, "status": "success", "ref": %q},
		"project": {"id": %d},
		"merge_request": {"iid": %d}
	}`, pipelineID, branchRef, projectID, mrIID)

	postWebhook(t, app, payload)
	waitFor(t, "note created", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.notes) == 1
	})
	glMock.mu.Lock()
	body := glMock.notes[0]
	glMock.mu.Unlock()

	// The reported pipeline's job ran from -10m to -5m, i.e. 5m, against the 4m
	// median of pipelines 901-903 (2m, 4m, 6m): +1m, +25%. If either excluded
	// pipeline (30m on branchRef, 40m on the MR ref) had leaked into the sample
	// set, the median — and this delta — would be far larger.
	const wantDelta = "🔺 +1m 00s (+25%)"
	if !strings.Contains(body, wantDelta) {
		t.Errorf("report is missing the expected delta %q:\n%s", wantDelta, body)
	}
	if !strings.Contains(body, "| Duration |") {
		t.Errorf("details table is missing the Duration column:\n%s", body)
	}
	// Three samples is a thin baseline, so the footnote must name the count.
	if !strings.Contains(body, "median of 3 recent successful pipelines") {
		t.Errorf("thin-baseline footnote missing:\n%s", body)
	}
}
```

The baseline job listings carry no `stage` field, exactly like the reported pipeline's job, so both sides key on `JobKey{Stage: "", Name: "build"}` and the per-job delta matches the pipeline one.

- [ ] **Step 4: Run the e2e suite**

Run: `mise r test:e2e`
Expected: PASS. If the median or percentage differs, print `body` and recompute from the mock's numbers — the mock's reported job window is `-10m` to `-5m`, i.e. 5 minutes, so adjust the expected delta rather than the implementation if the fixture changed.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e
git commit -m "test(e2e): assert the duration baseline and ref exclusion end to end"
```

---

## Task 10: Helm chart

**Files:**
- Modify: `deploy/chart/cigar/values.yaml`
- Modify: `deploy/chart/cigar/templates/configmap.yaml`
- Modify: `deploy/chart/cigar/tests/config_test.yaml`

- [ ] **Step 1: Write the failing chart test**

In `deploy/chart/cigar/tests/config_test.yaml`, extend the defaults assertion's expected `config.yaml` — insert into the `report:` block, after `memory_pressure_ratio`:

```yaml
              compare:
                enabled: "true"
                duration_delta_ratio: "0.05"
                history_pipelines: "6"
                cache_ttl: "1h"
```

(Indentation is two levels under `report:` inside the `value: |` block — match the surrounding lines exactly.)

Then add an override case at the end of the suite:

```yaml
  - it: propagates compare overrides into config.yaml
    set:
      config:
        report:
          compare:
            enabled: false
            durationDeltaRatio: "0.10"
            historyPipelines: "12"
            cacheTtl: "30m"
    asserts:
      - matchRegex:
          path: data["config.yaml"]
          pattern: 'enabled: "false"'
      - matchRegex:
          path: data["config.yaml"]
          pattern: 'duration_delta_ratio: "0.10"'
      - matchRegex:
          path: data["config.yaml"]
          pattern: 'history_pipelines: "12"'
      - matchRegex:
          path: data["config.yaml"]
          pattern: 'cache_ttl: "30m"'
```

- [ ] **Step 2: Run the chart tests to verify they fail**

Run: `mise r helm:test`
Expected: FAIL — the rendered `config.yaml` has no `compare:` block.

- [ ] **Step 3: Add the values**

In `deploy/chart/cigar/values.yaml`, inside `config.report`, after `memoryPressureRatio`:

```yaml
    # Compare this pipeline's duration against recent successful pipelines on
    # other refs, annotating changes beyond durationDeltaRatio. historyPipelines
    # must be at least 3 when enabled.
    compare:
      enabled: true
      durationDeltaRatio: "0.05"
      historyPipelines: "6"
      cacheTtl: "1h"
```

- [ ] **Step 4: Render it in the ConfigMap**

In `deploy/chart/cigar/templates/configmap.yaml`, inside the `report:` block after `memory_pressure_ratio`:

```yaml
      compare:
        enabled: {{ .Values.config.report.compare.enabled | quote }}
        duration_delta_ratio: {{ .Values.config.report.compare.durationDeltaRatio | quote }}
        history_pipelines: {{ .Values.config.report.compare.historyPipelines | quote }}
        cache_ttl: {{ .Values.config.report.compare.cacheTtl | quote }}
```

- [ ] **Step 5: Run the chart tests to verify they pass**

Run: `mise r helm:test`
Expected: PASS — `helm lint` clean and every suite green.

- [ ] **Step 6: Commit**

```bash
git add deploy/chart/cigar
git commit -m "feat(helm): expose the report.compare settings"
```

---

## Task 11: Documentation

**Files:**
- Modify: `docs/usage.md`
- Modify: `docs/deploy.md`
- Modify: `README.md`

- [ ] **Step 1: Document the env vars**

In `docs/usage.md`'s environment table, after the `REPORT_MEMORY_PRESSURE_RATIO` row:

```md
| `REPORT_COMPARE_ENABLED` | no | `true` | Compare pipeline/job durations against the median of recent successful pipelines on other refs |
| `REPORT_COMPARE_DURATION_DELTA_RATIO` | no | `0.05` | Relative duration change above which the report annotates a delta (e.g. `🔺 +2m 08s (+51%)`) |
| `REPORT_COMPARE_HISTORY_PIPELINES` | no | `6` | How many recent successful pipelines form the baseline; minimum `3` when enabled |
| `REPORT_COMPARE_CACHE_TTL` | no | `1h` | How long a computed baseline is cached per project+branch; `0` disables caching |
```

- [ ] **Step 2: Explain the comparison**

Add a short subsection to `docs/usage.md` near the report-content description:

```md
### Duration comparison

The report compares this pipeline's wall clock — and each job's duration —
against the **median of the last `REPORT_COMPARE_HISTORY_PIPELINES` successful
pipelines of the project**, excluding pipelines on the reported branch (both the
branch ref and `refs/merge-requests/<iid>/head`). Comparing a branch against its
own earlier runs would measure iteration noise; excluding it answers the useful
question, "does this change build slower than other code?".

A delta is shown only when the change exceeds `REPORT_COMPARE_DURATION_DELTA_RATIO`
and at least 3 baseline pipelines exist, so new projects and newly added jobs
show plain durations rather than confident-looking noise. When the baseline is
thin (3–5 pipelines) the report footnotes the actual sample count.

Baselines are cached per project+branch for `REPORT_COMPARE_CACHE_TTL`, so the
comparison costs roughly one pipeline listing plus one job listing per baseline
pipeline, once an hour. Set `REPORT_COMPARE_ENABLED=false` to switch the feature
— and all of its API calls — off.
```

- [ ] **Step 3: Update the chart reference**

In `docs/deploy.md`'s values example, inside `report:` after `memoryPressureRatio`:

```yaml
    compare:
      enabled: true
      durationDeltaRatio: "0.05"
      historyPipelines: "6"
      cacheTtl: "1h"
```

- [ ] **Step 4: Refresh the README report sample**

The definition of done requires the README's report sample to match the new format. Regenerate it from the golden file:

```bash
sed -n '/### Details/,$p' internal/report/testdata/report.md
```

Update the README's sample report block so its details table shows the `Duration` column and its summary row shows the pipeline delta. If the README embeds a screenshot image rather than markdown, note in the PR description that the screenshot needs regenerating and update the surrounding text to mention the duration comparison.

- [ ] **Step 5: Verify the docs render and links hold**

Run: `grep -rn "REPORT_COMPARE" docs/ README.md`
Expected: the four variables appear in `docs/usage.md`, and `docs/usage.md`'s new subsection references them.

- [ ] **Step 6: Commit**

```bash
git add docs README.md
git commit -m "docs: document the duration comparison and its settings"
```

---

## Task 12: Full verification

- [ ] **Step 1: Lint and test with the race detector**

Run: `mise r lint test`
Expected: both clean. `TestOversizedBodyRejected` in `internal/webhook` is a known macOS flake (passes on Linux CI) — if it is the only failure, re-run it alone to confirm and move on.

- [ ] **Step 2: Chart suite**

Run: `mise r helm:test`
Expected: PASS.

- [ ] **Step 3: Confirm nothing else queries the pipelines endpoint when disabled**

Run: `go test -race ./internal/reporter/ -run TestBuildWithoutHistorySourceSkipsComparison -v`
Expected: PASS — proof that `report.compare.enabled=false` costs zero API calls.

- [ ] **Step 4: Review the diff against the spec**

Run: `git diff main --stat`
Expected: changes confined to `internal/{history,report,reporter,gitlab,config,e2e}`, `cmd/bot`, `deploy/chart/cigar`, `docs`, `README.md`.

- [ ] **Step 5: Commit anything outstanding**

```bash
git status --short
```

Expected: clean. If not, commit the remainder with a message describing it.
