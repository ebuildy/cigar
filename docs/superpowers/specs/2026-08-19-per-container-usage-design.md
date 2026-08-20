# Per-container usage: build, helper and services

Date: 2026-08-19

## Goal

A GitLab Kubernetes runner pod is not one container. It runs:

- `build` — the job's own `script:`,
- `helper` — the runner's plumbing: git clone, artifacts, cache upload/download,
- one container per CI `services:` entry (dind, postgres…), named after the
  entry's `alias:` when the job sets one and positional `svc-N` otherwise,
- and, injected by the runner, an `init-permissions` init container.

Today every number in the report is a `sum()` over all of them, so a job row
silently mixes the user's work with the runner's plumbing. Worse, a helper
container starved of CPU — which stalls clone and cache, inflating wall-clock
time — is invisible: its throttling is averaged into the job's ratio and the
advice suggests `KUBERNETES_CPU_LIMIT`, the wrong variable.

This design makes the container breakdown visible and makes CPU-throttling
advice name the container it is about, with the right GitLab CI variable.

## What changes, in one sentence each

1. `metrics.JobUsage` gains a per-container breakdown; the job-level totals keep
   their exact current meaning and value.
2. The Details table nests one row per container under each job, when the
   pipeline is small enough that the table stays readable.
3. The `cpu-throttle` advice rule fires **per throttled container**, each with
   the CI variable that actually governs that container.
4. `details job <name>` replies with the per-container table, so the breakdown
   is reachable on pipelines too large to show it inline.

## Non-goals

- **Changing what a job row means.** The job row stays the pod total. Attributing
  usage to `build` only was considered and rejected: a `dind`-heavy job would
  report far less than the pod actually consumed, and the pipeline totals would
  stop matching what the cluster was asked for.
- **Per-container memory-pressure advice.** `memory-pressure` stays job-level.
  Only CPU throttling gained a per-container signal worth acting on.
- **Mapping an un-aliased `svc-N` to its image.** Recovering `postgres:16` from
  `svc-0` needs the job's CI config. The table renders the cadvisor name
  verbatim. (An *aliased* service needs no mapping — its container is already
  named after the alias.)
- **Per-container network.** cadvisor's `container_network_*` series are
  pod-level (no `container` label). Container rows render `—` for network, never
  a fabricated zero.

## `internal/metrics`

### Data model

```go
// ContainerUsage is one container's slice of a runner pod's usage.
// Network is absent by construction: cadvisor's network series are pod-level.
type ContainerUsage struct {
	Name string // cadvisor `container` label: build, helper, db, svc-0…

	CPUSeconds      float64
	PeakMemoryBytes uint64
	DiskReadBytes   uint64
	DiskWriteBytes  uint64

	// Raw CFS counters, not a ratio: the job-level ratio must stay
	// sum(throttled)/sum(periods), which a mean of ratios would not reproduce.
	ThrottledPeriods float64
	Periods          float64

	CPURequestCores    float64
	CPULimitCores      float64
	MemoryRequestBytes uint64
	MemoryLimitBytes   uint64
}

// ThrottledRatio is this container's throttled fraction. ok is false when the
// CFS period series was absent or zero — absent ≠ 0%.
func (c ContainerUsage) ThrottledRatio() (float64, bool)
```

`JobUsage` gains one field:

```go
// Containers is the per-container breakdown, ordered build, helper, un-aliased
// svc-N ascending, then any other name alphabetically. Runner init containers
// are excluded. Empty when the container-level series were absent; the totals
// above are then all that is known.
Containers []ContainerUsage
```

**Every existing `JobUsage` field keeps its current meaning and value** — the
pod total, pause container excluded. Nothing downstream that reads `JobUsage`
today has to change to keep working.

### Queries

The ten scalar queries in `PromSource.PodUsage` change from `sum(...)` to
`sum by (container) (...)`, and the two CFS queries likewise. The query **count
is unchanged** — twelve queries per job, exactly as today. The container
selector (`container!="",container!="POD"`) is unchanged.

The two network queries stay pod-level scalars.

`kube_pod_container_resource_requests` / `_limits` currently use the pod-level
selector with no container filter; they move to
`sum by (container)(... {pod="…",container!="",container!="POD"})`. The sum over
the returned containers equals today's pod-wide sum, so the job row's
`Mem req / limit` and `CPU req / limit` columns do not move.

### Totals are derived, never queried twice

`PodUsage` assembles `Containers` from the vector results, then computes every
`JobUsage` total by summing that slice:

- `CPUSeconds`, `PeakMemoryBytes`, `DiskReadBytes`, `DiskWriteBytes`,
  `CPURequestCores`, `CPULimitCores`, `MemoryRequestBytes`, `MemoryLimitBytes`
  — plain sums, arithmetically identical to today's `sum()` queries.
- `ThrottledRatio` = `Σ ThrottledPeriods / Σ Periods`, identical to today's
  `sum(throttled)/sum(periods)`.

This is the reason for one query pass rather than two: a total and its rows
computed by separate queries drift whenever a container enters or leaves the
window. Here they cannot disagree — the total *is* the sum of the rows.

An empty vector still means unset, not zero: a metric absent for every container
leaves the corresponding total at its zero value **without** being treated as a
measurement, exactly as the current `ok bool` from `scalar` does. `Containers`
is built from the union of container names seen across the result vectors, so a
container that reports memory but not disk gets a row with disk unset.

### Container ordering

`build`, then `helper`, then un-aliased `svc-N` by ascending numeric suffix,
then anything else (alias-named services) alphabetically. Stable order is a
golden-file requirement — Prometheus returns vector samples in no guaranteed
order.

## `internal/report`

### Nested rows

`Data` gains `ContainerDetailMaxJobs int`. The Details table nests container
rows under a job when **all** of:

- `ContainerDetailMaxJobs > 0` (0 disables the breakdown entirely),
- `len(d.Jobs) <= ContainerDetailMaxJobs`,
- that job has `len(Usage.Containers) > 1` — a single-container breakdown
  repeats the job row and earns nothing.

The count is every job in the report, including `_no data_ ` ones: they occupy a
table row too, and the threshold is about the table's height.

```md
| Stage : Job | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX | Disk R / W |
|---|---|---|---|---|---|---|---|
| build : compile | 42.5 s | 412.0 MiB | 256.0 MiB / 512.0 MiB | 250m / 500m | **41%** ⚠️ | 8.0 MiB / 3.0 MiB | 600.0 MiB / 220.0 MiB |
| ↳ build | 39.8 s | 380.0 MiB | 128.0 MiB / 256.0 MiB | 150m / 250m | 12% | — / — | 580.0 MiB / 200.0 MiB |
| ↳ helper | 2.3 s | 24.0 MiB | 128.0 MiB / 256.0 MiB | 100m / 250m | **58%** ⚠️ | — / — | 20.0 MiB / 20.0 MiB |
| ↳ db | 0.4 s | 8.0 MiB | — / — | — / — | 0% | — / — | — / — |
```

Container rows reuse the existing cell helpers (`dash` for absent series, the
`throttle` ⚠️ bolding against the same `ThrottleWarnRatio`). A container whose
CFS series was absent renders `—` in `Throttled`, not `0%`.

The pipeline **Summary** table is untouched: it already sums job totals, which
still cover every container.

### Configuration

One new setting in the `settings` table, deriving its three forms as usual:

| yaml | env | flag | default |
|---|---|---|---|
| `report.container_detail_max_jobs` | `REPORT_CONTAINER_DETAIL_MAX_JOBS` | `--report-container-detail-max-jobs` | `10` |

`Config.ContainerDetailMaxJobs int`, parsed by a new `parseInt` helper in
`internal/config` (rejecting negatives with the same error shape the other
parsers use). Wired through `cmd/bot/deps.go` into `reporter.Reporter`, which
sets it on `report.Data` alongside `ThrottleWarnRatio`.

## `internal/advice`

### `cpu-throttle` becomes per-container

`Facts.Usage.Containers` drives the rule:

- For each container whose `ThrottledRatio()` is known and `>= ThrottleWarnRatio`,
  emit one `Advice` — `Rule: "cpu-throttle"`, `Job: f.Name`,
  `Title: "⚠️ CPU throttling — <container>"`.
- Containers are visited in `Containers` order, so advice order is stable.
- **Fallback:** when `Containers` is empty (container-level series absent) the
  rule fires exactly as it does today, on the job-level ratio, with the
  `build`-container variables and the current title. Behavior on a Prometheus
  that cannot answer the breakdown is unchanged.

The shared `throttled(f, t)` predicate stays as the fallback's guard.

### Variables per container

| container | CPU request | CPU limit |
|---|---|---|
| `build` | `KUBERNETES_CPU_REQUEST` | `KUBERNETES_CPU_LIMIT` |
| `helper` | `KUBERNETES_HELPER_CPU_REQUEST` | `KUBERNETES_HELPER_CPU_LIMIT` |
| anything else | `KUBERNETES_SERVICE_CPU_REQUEST` | `KUBERNETES_SERVICE_CPU_LIMIT` |

**Classification is by exclusion, and this is load-bearing.** An earlier draft
of this spec matched services on a `svc-` prefix. Verified against the dev
cluster (2026-08-20, pipeline 25), that is wrong: the Kubernetes executor names
a service container after its `alias:` when the job sets one, and only falls
back to positional `svc-N` when it does not. One pod running
`postgres:16-alpine` aliased to `db` plus an un-aliased `redis:7-alpine`
produced containers `build helper db svc-0`. A prefix test silently handed the
`db` container the *build* variables. Since `build` and `helper` are the only
names the executor fixes, everything else is a service.

`KUBERNETES_SERVICE_*` applies to **every** service container — GitLab has no
per-service variable — and the advice body says so when it fires for one.

Runner-injected init containers (`init-permissions`, observed on the same
cluster) are dropped in `metrics`, before the breakdown is built: they run
before the job, no CI variable tunes them, and a row the reader cannot act on
is noise. They are excluded from the totals too, so the job row stays the sum
of the rows shown.

`suggestedCPULimit` / `suggestedCPURequest` are unchanged; they now take the
container's own request/limit instead of the pod-wide sums, which is what makes
the suggestion correct rather than merely plausible.

The helper's body also explains *why* it matters, since "the helper is slow" is
not self-evident: the helper runs `git clone`, artifact download/upload and
cache, so throttling it stretches every job's setup and teardown without
appearing in the job's own script time.

Full YAML blocks stay inline in the report, as today — one block per throttled
container.

## `internal/command`

`details job <name>` and `details pod <name>` already resolve a pod and window
and reply with three charts. The reply gains a per-container table above the
charts, rendered by the same helper the report uses (exported from
`internal/report` so the two cannot drift).

The handler needs `metrics.Source` in addition to its existing
`metrics.SeriesSource` — one `PodUsage` call for the resolved pod and window.
When that call fails, or returns no containers, the reply still posts the charts
and omits the table: a chart is worth more than an error.

`help` gains a line noting that `details` includes the container breakdown.

## Testing

- **`internal/metrics`** — table-driven tests over a stub Prometheus asserting:
  the emitted PromQL carries `by (container)`; a three-container vector produces
  three ordered `ContainerUsage` entries; the derived totals equal the sum of
  the parts; a container present in one vector and absent from another gets a
  row with the missing field unset; an empty vector leaves both the total and
  `Containers` untouched. New queries are verified against the Prometheus
  snapshot in `internal/metrics/testdata`, per the project rule.
- **`internal/report`** — golden files: one pipeline under the threshold (nested
  rows), one over it (no nested rows, identical to today's golden), one with a
  single-container job (not nested), one with `ContainerDetailMaxJobs: 0`.
- **`internal/advice`** — golden files per container kind: helper-only
  throttled, build + helper both throttled (two blocks, stable order),
  `svc-0` throttled (service variables + the "applies to all services" note),
  and the empty-`Containers` fallback reproducing today's output byte for byte.
- **`internal/config`** — `parseInt` accepts the default, rejects a negative and
  a non-numeric value; the flag > env > file > default precedence test covers
  the new key.
- **`internal/e2e`** — the mock Prometheus returns a two-container vector; the
  asserted note body contains the nested `↳ helper` row and the helper advice
  block. This is the proof the breakdown survives the whole chain.
- **README** — the report screenshot and `docs/usage.md` are updated for the new
  rows and the new setting, per the definition of done.

## Migration and compatibility

No stored state, no API surface, nothing to migrate. Existing deployments get
the breakdown on their next pipeline; a Prometheus without container-level
series falls back to exactly today's report and today's advice.
