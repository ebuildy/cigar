# GitLab-trace pod resolver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a second `correlate.Resolver` that finds a job's runner pod by parsing its GitLab trace log, selectable at startup via a new `POD_RESOLVER` env var (default `trace`).

**Architecture:** The `gitlab` package gains a `JobTrace` method wrapping client-go's `Jobs.GetTraceFile`. A new `correlate.traceResolver` depends on a tiny `traceFetcher` interface (satisfied by `gitlab.Client`), fetches the trace, and returns the first pod named in a `Running on <pod> via …` line (ANSI-tolerant). `cmd/bot/deps.go` switches on `config.PodResolver` to wire either the trace or the existing Prometheus resolver. Metrics still come from Prometheus regardless.

**Tech Stack:** Go 1.26, `gitlab.com/gitlab-org/api/client-go` v1.46.0, `go.uber.org/zap`, standard library (`bufio`, `regexp`, `io`).

**Spec:** [docs/superpowers/specs/2026-07-22-trace-pod-resolver-design.md](../specs/2026-07-22-trace-pod-resolver-design.md)

---

## File Structure

- **Modify** `internal/gitlab/client.go` — add `JobTrace` to the `Client` interface.
- **Modify** `internal/gitlab/gitlab.go` — implement `JobTrace` on `apiClient`.
- **Create** `internal/gitlab/gitlab_trace_test.go` — httptest coverage for `JobTrace`.
- **Create** `internal/correlate/trace.go` — `traceResolver` + `NewTraceResolver` + `traceFetcher` interface + regexes.
- **Create** `internal/correlate/trace_test.go` — table-driven parse coverage with a stub `traceFetcher`.
- **Modify** `internal/config/config.go` — add `PodResolver` field, load + validate `POD_RESOLVER`.
- **Modify** `internal/config/config_test.go` — cover `POD_RESOLVER` default/valid/invalid.
- **Modify** `cmd/bot/deps.go` — switch resolver on `cfg.PodResolver`.
- **Modify** `internal/e2e/e2e_test.go` — serve the job trace endpoint; add a trace-resolver assertion.
- **Modify** `CLAUDE.md` — document `POD_RESOLVER` in the Config section.

---

## Task 1: `gitlab.Client.JobTrace`

**Files:**
- Modify: `internal/gitlab/client.go` (add method to `Client` interface)
- Modify: `internal/gitlab/gitlab.go` (implement on `apiClient`, add `io` import)
- Test: `internal/gitlab/gitlab_trace_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/gitlab/gitlab_trace_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitlab/ -run TestJobTrace -v`
Expected: FAIL — compile error, `c.JobTrace undefined (type Client has no field or method JobTrace)`.

- [ ] **Step 3: Add `JobTrace` to the `Client` interface**

In `internal/gitlab/client.go`, add to the `Client` interface (after `UpsertNote`):

```go
	// JobTrace returns the raw trace log of a job.
	JobTrace(ctx context.Context, projectID, jobID int64) (string, error)
```

- [ ] **Step 4: Implement `JobTrace` on `apiClient`**

In `internal/gitlab/gitlab.go`, add `"io"` to the imports, then add the method (after `UpsertNote`):

```go
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
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/gitlab/ -run TestJobTrace -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/gitlab/client.go internal/gitlab/gitlab.go internal/gitlab/gitlab_trace_test.go
git commit -m "feat(gitlab): add JobTrace to fetch a job's raw trace log"
```

---

## Task 2: `correlate.traceResolver` — trace parsing

**Files:**
- Create: `internal/correlate/trace.go`
- Test: `internal/correlate/trace_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/correlate/trace_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/correlate/ -run TestTraceResolver -v`
Expected: FAIL — compile error, `undefined: NewTraceResolver`.

- [ ] **Step 3: Write the implementation**

Create `internal/correlate/trace.go`:

```go
package correlate

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// traceFetcher is the slice of the GitLab client the trace resolver needs;
// gitlab.Client satisfies it.
type traceFetcher interface {
	JobTrace(ctx context.Context, projectID, jobID int64) (string, error)
}

// ansiRE strips SGR color escape codes GitLab wraps trace lines in.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// runnerLineRE captures the pod hostname from the runner's "Running on <pod>
// via <manager>" trace line (Kubernetes executor: <pod> is the build pod).
var runnerLineRE = regexp.MustCompile(`Running on (\S+) via `)

// NewTraceResolver returns a Resolver that finds the runner pod by parsing the
// job's GitLab trace log instead of querying Prometheus pod labels.
func NewTraceResolver(f traceFetcher, log *zap.Logger) Resolver {
	log.Debug("gitlab trace pod resolver created")
	return &traceResolver{fetch: f, log: log}
}

type traceResolver struct {
	fetch traceFetcher
	log   *zap.Logger
}

func (r *traceResolver) PodForJob(ctx context.Context, projectID, jobID int64, _, _ time.Time) (string, bool, error) {
	r.log.Debug("resolving pod from job trace", zap.Int64("job_id", jobID))
	trace, err := r.fetch.JobTrace(ctx, projectID, jobID)
	if err != nil {
		return "", false, fmt.Errorf("fetch trace for job %d: %w", jobID, err)
	}
	scanner := bufio.NewScanner(strings.NewReader(trace))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := ansiRE.ReplaceAllString(scanner.Text(), "")
		if m := runnerLineRE.FindStringSubmatch(line); m != nil {
			r.log.Debug("resolved pod from trace", zap.Int64("job_id", jobID), zap.String("pod", m[1]))
			return m[1], true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("scan trace for job %d: %w", jobID, err)
	}
	r.log.Debug("no runner line in job trace", zap.Int64("job_id", jobID))
	return "", false, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/correlate/ -run TestTraceResolver -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/correlate/trace.go internal/correlate/trace_test.go
git commit -m "feat(correlate): add GitLab-trace pod resolver"
```

---

## Task 3: `POD_RESOLVER` config

**Files:**
- Modify: `internal/config/config.go` (add field, load, validate)
- Test: `internal/config/config_test.go` (add cases)

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestLoadPodResolver(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "default is trace", env: "", want: "trace"},
		{name: "explicit trace", env: "trace", want: "trace"},
		{name: "explicit prometheus", env: "prometheus", want: "prometheus"},
		{name: "unknown value errors", env: "bogus", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITLAB_TOKEN", "tok")
			t.Setenv("PROMETHEUS_URL", "http://prom")
			t.Setenv("POD_RESOLVER", tt.env)

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() with POD_RESOLVER=%q: want error, got %+v", tt.env, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PodResolver != tt.want {
				t.Fatalf("PodResolver = %q, want %q", cfg.PodResolver, tt.want)
			}
		})
	}
}
```

Note: `t.Setenv("POD_RESOLVER", "")` sets it to empty, so `getenv` falls back to the default — the "default is trace" case is valid.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadPodResolver -v`
Expected: FAIL — compile error, `cfg.PodResolver undefined`.

- [ ] **Step 3: Add the field, load, and validation**

In `internal/config/config.go`, add to the `Config` struct (after `OpsAddr`):

```go
	PodResolver string
```

In `Load()`, add to the struct literal (after `OpsAddr: getenv("OPS_ADDR", ":8081"),`):

```go
		PodResolver: getenv("POD_RESOLVER", "trace"),
```

After the `SCRAPE_INTERVAL` block and before the required-vars loop, add validation:

```go
	if !validPodResolvers[cfg.PodResolver] {
		return nil, fmt.Errorf("POD_RESOLVER must be one of prometheus, trace, got %q", cfg.PodResolver)
	}
```

Add the allow-set near `validAuthMethods`:

```go
var validPodResolvers = map[string]bool{"prometheus": true, "trace": true}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestLoadPodResolver -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add POD_RESOLVER (default trace)"
```

---

## Task 4: Wire resolver selection in `cmd/bot`

**Files:**
- Modify: `cmd/bot/deps.go` (switch on `cfg.PodResolver` in `newReporter`)

This task has no unit test of its own (it is wiring); Task 5 exercises the trace path end-to-end. Verify by build.

- [ ] **Step 1: Replace the resolver construction**

In `cmd/bot/deps.go`, inside `newReporter`, replace this block:

```go
	resolver, err := correlate.NewPromResolver(cfg.PrometheusURL, cfg.ScrapeInterval, log)
	if err != nil {
		return nil, err
	}
```

with:

```go
	var resolver correlate.Resolver
	switch cfg.PodResolver {
	case "trace":
		resolver = correlate.NewTraceResolver(gl, log)
	case "prometheus":
		resolver, err = correlate.NewPromResolver(cfg.PrometheusURL, cfg.ScrapeInterval, log)
		if err != nil {
			return nil, err
		}
	}
```

(`gl` is the `gitlab.Client` already built above; `err` is already declared from `gitlab.New`.)

- [ ] **Step 2: Build to verify it compiles**

Run: `go build ./cmd/bot`
Expected: no output (success).

- [ ] **Step 3: Run the full package tests**

Run: `go test ./cmd/... ./internal/... 2>&1 | tail -20`
Expected: all `ok` / no failures.

- [ ] **Step 4: Commit**

```bash
git add cmd/bot/deps.go
git commit -m "feat(bot): select pod resolver from POD_RESOLVER"
```

---

## Task 5: e2e coverage for the trace resolver

**Files:**
- Modify: `internal/e2e/e2e_test.go` (serve trace endpoint; assert trace path drives pod-filtered queries)

The existing `harness` wires `NewPromResolver`. Add a parameter so a test can select the trace resolver, serve the trace endpoint from the mock GitLab, and assert usage queries are filtered by the pod parsed from the trace.

- [ ] **Step 1: Add a trace fixture to the mock GitLab server**

In `internal/e2e/e2e_test.go`, inside `(*mockGitLab).server`, add a handler alongside the existing job/notes handlers (before the catch-all `mux.HandleFunc("/", …)`):

```go
	mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/jobs/%d/trace", projectID, jobID),
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "Preparing environment\nRunning on %s via gitlab-runner-mgr...\nJob succeeded\n", podName)
		})
```

- [ ] **Step 2: Parameterize `harness` to select the resolver**

Change the `harness` signature and the resolver construction. Replace:

```go
func harness(t *testing.T) (*fiber.App, *mockGitLab, *mockProm) {
```

with:

```go
func harness(t *testing.T, podResolver string) (*fiber.App, *mockGitLab, *mockProm) {
```

Replace the existing resolver block:

```go
	resolver, err := correlate.NewPromResolver(promSrv.URL, 30*time.Second, log)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
```

with:

```go
	var resolver correlate.Resolver
	switch podResolver {
	case "trace":
		resolver = correlate.NewTraceResolver(glClient, log)
	default:
		var err error
		resolver, err = correlate.NewPromResolver(promSrv.URL, 30*time.Second, log)
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
	}
```

Note: `glClient` is already built above the resolver block. The `err` from `gitlab.New` is consumed there; the `default` branch declares its own `err` to avoid shadowing issues — keep it as written.

- [ ] **Step 3: Update existing callers**

Update both existing `harness(t)` calls to pass the Prometheus resolver:
- In `TestWebhookToMRNote`: `app, glMock, promMock := harness(t, "prometheus")`
- In `TestWebhookBranchResolvesMR`: `app, glMock, _ := harness(t, "prometheus")`

- [ ] **Step 4: Run existing e2e tests to verify they still pass**

Run: `go test ./internal/e2e/ -run 'TestWebhookToMRNote|TestWebhookBranchResolvesMR' -v`
Expected: PASS (parameterization is behaviour-preserving).

- [ ] **Step 5: Write the new trace-resolver e2e test**

Add to `internal/e2e/e2e_test.go`:

```go
// TestWebhookTraceResolver drives the full chain with POD_RESOLVER=trace: the
// pod is parsed from the job's GitLab trace, and usage queries must be filtered
// by that pod name.
func TestWebhookTraceResolver(t *testing.T) {
	app, glMock, promMock := harness(t, "trace")

	payload := fmt.Sprintf(`{
		"object_kind": "pipeline",
		"object_attributes": {"id": %d, "status": "success"},
		"project": {"id": %d},
		"merge_request": {"iid": %d}
	}`, pipelineID, projectID, mrIID)

	postWebhook(t, app, payload)
	waitFor(t, "note created via trace-resolved pod", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.notes) == 1
	})

	if !promMock.sawQuery(podName) {
		t.Error("usage queries were not filtered by the trace-resolved pod name")
	}
	if promMock.sawQuery("kube_pod_labels") {
		t.Error("trace resolver must not issue kube_pod_labels queries")
	}
}
```

- [ ] **Step 6: Run the new test**

Run: `go test ./internal/e2e/ -run TestWebhookTraceResolver -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/e2e/e2e_test.go
git commit -m "test(e2e): cover POD_RESOLVER=trace pod correlation"
```

---

## Task 6: Document `POD_RESOLVER` and full verification

**Files:**
- Modify: `CLAUDE.md` (Config section)

- [ ] **Step 1: Document the env var**

In `CLAUDE.md`, in the `### Config (env only, 12-factor)` section, add `POD_RESOLVER` to the variable list. Insert after `PROMETHEUS_URL`:

```
`POD_RESOLVER` (default `trace`; `trace` parses the job's GitLab trace for the
`Running on <pod> via …` line, `prometheus` joins `kube_pod_labels{label_job_id}`),
```

- [ ] **Step 2: Run the full test suite with the race detector**

Run: `mise r test`
Expected: all packages `ok`, no race warnings, e2e included.

- [ ] **Step 3: Lint**

Run: `mise r lint`
Expected: no findings.

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: document POD_RESOLVER env var"
```

---

## Definition of done

- `mise r lint test` clean, race detector on.
- `POD_RESOLVER=trace` (default) resolves the pod from the GitLab trace; `POD_RESOLVER=prometheus` preserves the existing label-join path.
- e2e proves the trace path drives pod-filtered metric queries and issues no `kube_pod_labels` query.
- Invalid `POD_RESOLVER` fails fast at startup.
- `CLAUDE.md` documents the new variable.
