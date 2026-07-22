# GitLab-trace pod resolver — design

Date: 2026-07-22

## Problem

`correlate.Resolver.PodForJob` maps a GitLab job to the Kubernetes runner pod
that executed it. The only implementation today is the Prometheus resolver
([`internal/correlate/prom.go`](../../../internal/correlate/prom.go)), which
joins `kube_pod_labels{label_job_id="<id>"}` and reads the `pod` label. That
strategy only works when the runner injects a `job_id` pod label that
kube-state-metrics exposes — not all runner configurations do.

GitLab job traces always contain a line naming the pod that ran the job, e.g.:

```
Running on runner-5fbdek91-project-3-concurrent-1-tyqut4ic via gitlab-runner-79f44bb98f-n7bbw...
```

For the Kubernetes executor the token after `Running on ` (here
`runner-5fbdek91-project-3-concurrent-1-tyqut4ic`) is the build pod's hostname,
which equals the cadvisor `pod` label. We can resolve the pod straight from the
trace, with no dependency on pod-label scraping.

## Goal

Add a second `correlate.Resolver` implementation that fetches a job's trace via
the GitLab API and parses that line, and select the active resolver at startup
via a new `POD_RESOLVER` env var.

## Scope

- This resolver handles **pod-name correlation only**. Resource metrics
  (`metrics.Source.PodUsage`) still come from Prometheus regardless of the
  selected resolver. `PROMETHEUS_URL` therefore remains required.
- No chained/fallback behaviour — exactly one resolver is active per process.

## Design

### 1. GitLab client — new `JobTrace` method

Add one method to the `gitlab.Client` interface
([`internal/gitlab/client.go`](../../../internal/gitlab/client.go)), following
the existing wrapper pattern used by `PipelineJobs`/`UpsertNote`:

```go
// JobTrace returns the raw trace log of a job.
JobTrace(ctx context.Context, projectID, jobID int64) (string, error)
```

Implemented in [`internal/gitlab/gitlab.go`](../../../internal/gitlab/gitlab.go)
with the client-go `Jobs` service:

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
    return string(b), nil
}
```

`GetTraceFile` returns a `*bytes.Reader` (the whole trace is already in memory),
so reading it fully is acceptable; the resolver still stops scanning at the
first match.

### 2. `traceResolver` — new `internal/correlate/trace.go`

Depends on a minimal interface `correlate` owns (satisfied by `gitlab.Client`),
so the resolver stays decoupled from the full GitLab surface and is trivially
stubbable:

```go
type traceFetcher interface {
    JobTrace(ctx context.Context, projectID, jobID int64) (string, error)
}

func NewTraceResolver(f traceFetcher, log *zap.Logger) Resolver
```

`PodForJob(ctx, projectID, jobID, _, _)` ignores the time window (not needed):

1. `trace, err := f.JobTrace(ctx, projectID, jobID)` — API error is wrapped and
   returned.
2. Scan `trace` line-by-line with `bufio.Scanner`. For each line: strip ANSI
   escape codes, then test against `runnerLineRE`.
3. First line that matches → return capture group 1, `ok=true`. **Stop
   immediately** (do not scan the rest of the trace).
4. No line matches → return `("", false, nil)`. Consistent with the Prometheus
   resolver: absent ≠ error, and a nil `Usage` is what the reporter expects.

Regexes (package-level, compiled once):

```go
var ansiRE       = regexp.MustCompile("\x1b\\[[0-9;]*m")
var runnerLineRE = regexp.MustCompile(`Running on (\S+) via `)
```

Log at debug on resolve/no-match, mirroring `promResolver`.

### 3. Config — `POD_RESOLVER`

In [`internal/config/config.go`](../../../internal/config/config.go):

- New field `PodResolver string`.
- Loaded via `getenv("POD_RESOLVER", "trace")` — **default `trace`**.
- Validate against `{"prometheus", "trace"}`; unknown value fails fast at
  startup (`fmt.Errorf`), like `THROTTLE_WARN_RATIO`/`AUTH_METHODS`.
- `PROMETHEUS_URL` stays in the required-vars list (metrics still need it).

### 4. Wiring — `cmd/bot/deps.go`

`newReporter` selects the resolver on `cfg.PodResolver`:

```go
var resolver correlate.Resolver
switch cfg.PodResolver {
case "trace":
    resolver = correlate.NewTraceResolver(gl, log) // gl is the gitlab.Client
case "prometheus":
    resolver, err = correlate.NewPromResolver(cfg.PrometheusURL, cfg.ScrapeInterval, log)
    if err != nil {
        return nil, err
    }
}
```

No `*gl.Client` leaks out of the `gitlab` package — the already-built
`gitlab.Client` is passed straight into `NewTraceResolver`.

## Data flow

Unchanged except the correlation step:

```
reporter.jobUsage
  -> Resolver.PodForJob   (Prometheus label-join OR GitLab trace parse)
  -> pod name
  -> Metrics.PodUsage     (always Prometheus)
```

## Testing

- `internal/correlate/trace_test.go` — table-driven, stub `traceFetcher`
  returning canned trace strings. Cases:
  - clean `Running on … via …` line → pod extracted;
  - line wrapped in ANSI color codes → still extracted;
  - target line not first in the trace → still found;
  - multiple `Running on …` lines → first one wins;
  - no matching line → `ok=false`, no error;
  - `JobTrace` returns an error → error propagated.
  Uses `zap.NewNop()`.
- `internal/config/config_test.go` — `POD_RESOLVER` default is `trace`; explicit
  `prometheus` accepted; unknown value → error.
- `internal/e2e` — extend the mock GitLab server to serve
  `GET /projects/:id/jobs/:job/trace` returning a fixture trace, and run the
  chain under `POD_RESOLVER=trace` to prove pod-filtered metric queries still
  work end-to-end.

## Documentation

- Add `POD_RESOLVER` to the config section in `CLAUDE.md` and any env-var docs
  under `docs/`.

## Out of scope

- Chained/fallback resolvers (prometheus → trace or vice-versa).
- Pod-name-pattern fallback (the existing `TODO` in `prom.go`).
- Any change to metric queries or report rendering.
