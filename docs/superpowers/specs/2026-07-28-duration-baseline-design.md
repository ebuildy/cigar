# Duration baseline: is this build slower than usual?

Date: 2026-07-28

## Goal

Answer one question in the MR report: **does this code change take longer to
build than recent builds of other code?** The report gains a duration
comparison at the pipeline level and a per-job `Duration` column, each annotated
with a delta when it moves by more than 5%.

This is a rough signal, not a benchmark. The design deliberately trades
precision and API-call efficiency for a small amount of code.

## Baseline definition

The baseline is the **median** duration of the **N most recent successful
pipelines of the project, across all refs, excluding the refs of the pipeline
being reported** (N = `report.compare.history_pipelines`, default 6).

Excluding the reporting refs is the point of the feature: comparing a branch
against its own earlier pipelines measures iteration noise, not whether the
change is slower. One branch can produce pipelines under two ref forms —
`feature-x` for branch pipelines and `refs/merge-requests/42/head` for detached
MR pipelines — so the caller supplies **every** ref identity it knows and the
history package excludes them all.

Median, not mean: one pathological run (cold cache, flaky retry, noisy node)
must not move the reference.

Durations use the definitions already in the report, so a delta is always
measured against the number printed next to it:

- pipeline duration = `max(job finish) − min(job start)` (`report.Data.wallClock`)
- job duration = `FinishedAt − StartedAt`

GitLab's own `pipeline.duration` field is **not** used: it excludes queue gaps
and would not match the wall clock displayed in the summary.

## Architecture

A new package `internal/history` owns "what did this normally take". It depends
only on `gitlab.Client`; `internal/report` gains no new dependency and stays a
pure renderer under golden-file tests.

```txt
reporter.Build ──> history.Source.Baseline(projectID, excludeRefs)
                        │
                        ├── cache hit (< cache_ttl) ──> Baseline
                        └── miss: gitlab.RecentSuccessfulPipelines (scan)
                                   └── gitlab.PipelineJobs × N  ──> medians
```

### `internal/history`

```go
// JobKey identifies a job across pipelines.
type JobKey struct{ Stage, Name string }

// Stat is a median duration and how many pipelines backed it.
type Stat struct {
    Median  time.Duration
    Samples int
}

// Baseline is the typical duration of a pipeline and of each of its jobs.
type Baseline struct {
    Pipeline Stat
    Jobs     map[JobKey]Stat
}

// Source is the boundary the reporter depends on; tests stub it.
type Source interface {
    // Baseline returns typical durations from recent successful pipelines,
    // ignoring any pipeline whose ref is in excludeRefs.
    Baseline(ctx context.Context, projectID int64, excludeRefs []string) (Baseline, error)
}

// Fetcher implements Source against the GitLab API, with a TTL cache.
type Fetcher struct {
    GitLab    gitlab.Client
    Pipelines int           // baseline size (>= minSamples, validated at load)
    TTL       time.Duration // cache entry lifetime, 0 disables caching
    Log       *zap.Logger   // named "history"
    // now is injectable for tests; defaults to time.Now.
    now func() time.Time
}
```

The off switch is `report.compare.enabled`, handled at wiring time: when it is
false, `serve` and `bot run` leave `Reporter.History` **nil** and `Build` skips
the lookup entirely — no `Fetcher`, no API calls, no cache. `Fetcher` itself
therefore never has to represent a disabled state.

Sampling rules:

- Scan width is `3 × Pipelines`, not configurable: GitLab's pipelines endpoint
  cannot express "ref ≠ X", so filtering happens client-side and needs slack.
- After filtering excluded refs, the newest `Pipelines` survivors are the sample
  set.
- A sample pipeline contributes a wall clock only if at least one of its jobs
  has both a start and a finish; pipelines with no run window are skipped.
- Within one sample pipeline, a `JobKey` appearing more than once (a retry)
  contributes only its **last-finishing** occurrence.
- A `Stat` with fewer than **3** samples is not produced. `minSamples = 3` is a
  constant, not a knob.
- Median of an even sample count is the mean of the two middle values.
- An empty `excludeRefs` filters nothing and caches under the empty ref. It is a
  valid call (a caller that could not resolve a ref), not an error.

### Cache

One map, one entry per `{projectID, ref}` where `ref` is the first (primary)
excluded ref, holding the already-reduced `Baseline` plus its fetch time.
Nothing raw is cached.

- Lookups evict their own entry when older than `TTL`.
- Capped at 500 entries; when full, the oldest entry is evicted before insert.
- Guarded by one `sync.Mutex`. Two pipelines of the same project finishing at
  once may both refill — harmless duplicate work, so no `singleflight`.
- Fetches are sequential: it is the async worker, the cost is hourly per
  project-branch, and sequential is gentler on GitLab rate limits.

Consequence, accepted: a different MR on the same project pays its own refill
(~7 API calls per project-branch per hour at the default 6) rather than sharing a per-pipeline
memo. That duplicate work buys markedly less code.

### `internal/gitlab`

One new `Client` method, plus one used only by `bot run`:

```go
// Pipeline is a past pipeline of the project, for duration baselines.
type Pipeline struct {
    ID  int64
    Ref string
}

// RecentSuccessfulPipelines returns the most recent successful pipelines of the
// project, newest first, across all refs.
RecentSuccessfulPipelines(ctx context.Context, projectID int64, limit int) ([]Pipeline, error)

// PipelineRef returns the ref a pipeline ran on. Used by `bot run`, which has
// no webhook payload to read it from.
PipelineRef(ctx context.Context, projectID, pipelineID int64) (string, error)
```

`RecentSuccessfulPipelines` calls `GET /projects/:id/pipelines?status=success`
with `per_page` covering `limit` (newest first, GitLab's default `id` descending
order) and paginates only if needed to reach `limit`.

### `internal/reporter`

`Reporter` gains `History history.Source`. `Build` takes the excluded refs from
its caller:

```go
func (r *Reporter) Build(ctx context.Context, projectID, pipelineID int64, excludeRefs []string) (report.Data, error)
```

- `ProcessPipeline` passes the webhook's `ref` (the source branch, for both
  branch and MR pipelines) plus, when `mrIID > 0`, the detached-MR ref form
  `refs/merge-requests/<mrIID>/head` — which the pipelines API reports for MR
  pipelines. Deriving the second form from `mrIID` costs no API call. The
  webhook `ref` is always first, making it the cache key.
- `bot run` calls `PipelineRef` and passes that single ref.
- A `Baseline` error is logged at **warn** (logger named `history`, so it lands
  in `cigar_log_total`) and the report renders **without deltas**. Only the jobs
  listing failing aborts a report — the existing rule is unchanged. No new
  telemetry metric.

### `internal/report`

New plain-value fields; no import of `history`:

```go
type JobReport struct {
    // …existing fields
    BaselineDuration time.Duration // 0 = no comparable baseline
    BaselineSamples  int
}

type Data struct {
    // …existing fields
    BaselinePipelineDuration time.Duration
    BaselinePipelineSamples  int
    DurationDeltaRatio       float64 // threshold, e.g. 0.05
}
```

Job duration for the pipeline being reported needs no new data:
`JobReport.StartedAt/FinishedAt` already exist.

## Rendering

```md
| Pipeline duration | 6m 20s 🔺 +2m 08s (+51%) |

| Stage : Job | Duration | CPU time | Peak memory | …
|---|---|---|---|
| build : compile  | 4m 30s 🔺 +1m 12s (+37%) | 42.5 s    | 412.0 MiB | …
| test : unit      | 1m 50s                   | 18.0 s    | 150.0 MiB | …
| deploy : staging | —                        | _no data_ |           | …

_Duration deltas vs the median of 6 recent successful pipelines on other refs._
```

- `Duration` is a new column in **second** position, right after `Stage : Job`.
- A delta prints only when that row has `BaselineSamples >= 3` **and**
  `|current − median| / median > DurationDeltaRatio`. Otherwise the cell holds
  the bare duration — never `0%`, never `—`, consistent with "absent ≠ zero".
- Per-row gating is independent: a new job in an otherwise well-sampled pipeline
  shows its duration with no delta.
- A job with no run window renders `—` in `Duration`, and no delta.
- A zero or negative median, or a zero current duration, prints no delta (no
  division by zero, no infinite percentage).
- Sign is `+` or `−` (U+2212), `🔺` when slower, `🔻` when faster, percentage
  rounded to whole numbers, absolute delta via the existing `humanDuration`.
- The footnote appears **only** when the pipeline baseline is thin (3–9
  samples), naming the actual count, so a thin median is not over-trusted.

## Configuration

The feature owns one grouped block of `config.yaml`:

```yaml
report:
  compare:
    enabled: true
    duration_delta_ratio: 0.05
    history_pipelines: 6
    cache_ttl: 1h
```

Four keys added to the `settings` table in `internal/config` (the single source
of truth for the yaml/env/flag triple — `envName`/`flagName` are plain string
transforms, so the extra nesting level needs no machinery change):

| Key | Env | Flag | Default |
|---|---|---|---|
| `report.compare.enabled` | `REPORT_COMPARE_ENABLED` | `--report-compare-enabled` | `true` |
| `report.compare.duration_delta_ratio` | `REPORT_COMPARE_DURATION_DELTA_RATIO` | `--report-compare-duration-delta-ratio` | `0.05` |
| `report.compare.history_pipelines` | `REPORT_COMPARE_HISTORY_PIPELINES` | `--report-compare-history-pipelines` | `6` |
| `report.compare.cache_ttl` | `REPORT_COMPARE_CACHE_TTL` | `--report-compare-cache-ttl` | `1h` |

`Config` gains `CompareEnabled bool`, `CompareDurationDeltaRatio float64`,
`CompareHistoryPipelines int`, `CompareCacheTTL time.Duration`.
`duration_delta_ratio` parses through the existing `parseRatio`, `cache_ttl`
through `parseDuration`.

Validation, only when `enabled` is true: `history_pipelines` must be **≥ 3**
(`minSamples`) — a smaller baseline could never produce a comparison, so it
fails at load rather than silently rendering nothing. `cache_ttl` may be `0`
(no caching). With the default 6, the scan width is 18.

The Helm chart mirrors the block as `config.report.compare.enabled`,
`.durationDeltaRatio`, `.historyPipelines`, `.cacheTtl` in `values.yaml`,
rendered into `config.yaml` by `templates/configmap.yaml` under
`report: compare:` with their snake_case keys.

## Testing

- **`internal/history`** — table-driven against a stub `gitlab.Client`:
  ref filtering excludes both `feature-x` and `refs/merge-requests/N/head`
  forms; median over odd and even sample counts; fewer than 3 samples yields no
  `Stat`; a retried job name appearing twice in one pipeline takes the
  last-finishing occurrence; a pipeline with no run window is skipped; cache
  hit, miss and expiry via an injected clock; cap eviction.
- **`internal/report`** — golden files. `testdata/report.md` is regenerated with
  the `Duration` column (required by the definition of done), plus new goldens
  for a thin baseline (footnote present), a within-threshold pipeline (no delta
  anywhere), and no baseline at all.
- **`internal/reporter`** — a stub `history.Source` returning an error proves the
  report still builds and renders without deltas; another asserts the excluded
  refs passed through are the webhook ref plus the MR source branch.
- **`internal/e2e`** — the mock GitLab serves
  `GET /projects/:id/pipelines?status=success` and job lists for the baseline
  pipeline IDs. Asserts the upserted note carries a duration delta, and that a
  same-ref pipeline offered by the mock never influences it.
- **`internal/config`** — the four new keys added to the settings-table test,
  with parse cases and the `history_pipelines < 3` load failure (and proof that
  the same value is accepted when `enabled` is false).
- **`internal/reporter` / wiring** — `report.compare.enabled: false` leaves
  `Reporter.History` nil and the report renders with durations but no deltas and
  no pipeline-list API call.
- **Chart** — a helm-unittest case covering the `config.report.compare.*`
  block's rendering into `config.yaml`.
- **Docs** — `docs/deploy.md` (chart reference) and `docs/usage.md` document the
  new keys; the README report screenshot is refreshed for the new column.

## Out of scope

- Duration-based advice rules in `internal/advice` (e.g. "split this job"): the
  existing `report.long_job_duration` rule already covers long jobs; a
  regression-triggered rule is a separate feature.
- Charting duration history in command replies (`internal/chart`).
- Persisting history across restarts or sharing it between replicas.
- Comparing anything other than duration (CPU, memory) against a baseline.
