# Design: the `advise` command

Date: 2026-07-23
Status: approved, ready for planning

## Problem

The report note tells users *what* their pipeline consumed. It never tells them
*what to do about it*. `internal/report`'s package doc has claimed an "advice
engine" since the first commit; none exists.

Users want an on-demand second opinion: "look at this pipeline and tell me what
to fix". It must be available both as a reply to the report note (where the
audience already is) and from the CLI (where the maintainer debugs).

Above all, the rule set must grow cheaply: adding an adviser is a new file, not
a surgery on a `switch`.

## Solution overview

A new pure package `internal/advice` holding a **rule engine**. Orchestration
(jobs → pod → usage → trace → advice) lives in `internal/reporter`, so the note
command and the CLI share one code path, as `Build`/`Render` already do.

```txt
internal/advice/          rules + engine + markdown rendering (pure, no I/O)
internal/reporter/        Reporter.Advise: gathers Facts, runs the engine
internal/command/         KindAdvise -> Advisor interface -> reply
cmd/bot/advise.go         bot advise --project <id> <pipeline-id> [--job <name>]
```

The pipeline report note is unchanged. Advice is command-triggered only.

## 1. `internal/advice`

### Types

```go
// Facts is everything the rules know about one job. Trace is populated only
// when a rule asked for it (see TraceConsumer).
type Facts struct {
    Stage, Name string
    Duration    time.Duration
    Usage       *metrics.JobUsage // nil when the pod or its metrics were unavailable
    Trace       string
}

// Thresholds are the tunables shared by the rules.
type Thresholds struct {
    ThrottleWarnRatio   float64       // THROTTLE_WARN_RATIO, default 0.25
    LongJob             time.Duration // LONG_JOB_DURATION, default 10m
    MemoryPressureRatio float64       // MEMORY_PRESSURE_RATIO, default 0.9
}

// Advice is one actionable finding about one job.
type Advice struct {
    Job   string // job name; "" for pipeline-wide advice
    Rule  string // stable id, e.g. "cpu-throttle"
    Title string // one line, rendered as a bold heading
    Body  string // markdown, may be multi-line and contain links
}
```

### The Rule interface

```go
// Rule is one advice check: stateless, pure, independently testable.
type Rule interface {
    Name() string                          // stable id, matches Advice.Rule
    Check(f Facts, t Thresholds) []Advice  // nil when the rule does not fire
}

// TraceConsumer is the optional capability a rule implements when it needs the
// job's trace log. The engine asks every rule before any fetch happens; the
// trace is pulled only when at least one rule says yes, and Check then runs
// with Facts.Trace populated.
type TraceConsumer interface {
    NeedsTrace(f Facts, t Thresholds) bool
}
```

Rules **must not depend on each other**. A rule that logically follows another
(e.g. the Java-threads hint only makes sense for a throttled job) re-evaluates
that condition itself. No ordering coupling, no shared state.

`Check` takes no `context.Context` and returns no `error` — on purpose. All I/O
happens before the engine runs, which is what makes rules golden-testable.

### The engine

```go
type Engine struct {
    rules []Rule
    th    Thresholds
}

// New builds an engine. enabled == nil selects every registered rule in
// registration order; otherwise only the named rules, and an unknown name is an
// error. This is the seam for a future ADVICE_RULES env var.
func New(th Thresholds, enabled []string) (*Engine, error)

// Register appends a rule to the registry. Built-ins are registered from
// rules.go; the function is exported so out-of-tree rules can be added later.
func Register(r Rule)

func (e *Engine) NeedsTrace(f Facts) bool  // true iff some TraceConsumer wants it
func (e *Engine) Analyze(f Facts) []Advice // rules run in registration order
```

`rules.go` holds the ordered builtin list. **Adding an adviser is one new file
plus one line here:**

```go
var builtins = []Rule{
    cpuThrottle{}, javaThreads{}, longJob{}, memoryPressure{},
}
```

Registration order is the report order, so golden files stay stable.

`ADVICE_RULES` is deliberately *not* added in this change: the seam exists,
wiring it is a one-line follow-up when a user asks for it.

### Rendering

```go
// Render turns the pipeline's advice into the markdown body posted or printed.
// With no advice it returns exactly "You are all good dude!".
func Render(pipelineID int64, all []Advice) string
```

Layout: a `### Advice for pipeline #N` heading, then advice grouped by job in
job order, each finding as a bold title plus its body. Pipeline-wide advice
(`Advice.Job == ""`) renders first, before the per-job groups.

## 2. The built-in rules

| File | Rule name | Fires when | Advice |
| --- | --- | --- | --- |
| `cpu_throttle.go` | `cpu-throttle` | `Usage != nil && Usage.ThrottledRatio >= t.ThrottleWarnRatio` | Raise `KUBERNETES_CPU_REQUEST` / `KUBERNETES_CPU_LIMIT`, quoting the job's current values when the series were present |
| `java_threads.go` | `java-threads` | the throttle condition **and** `Trace` matches maven/gradle/java | Pin build parallelism to the CPU limit instead of the host core count |
| `long_job.go` | `long-job` | `Duration > t.LongJob` | Split the job: parallel jobs/stages, `parallel:`/matrix, better caching |
| `memory_pressure.go` | `memory-pressure` | `Usage != nil && Usage.MemoryLimitBytes > 0 && PeakMemoryBytes >= t.MemoryPressureRatio * MemoryLimitBytes` | Raise `KUBERNETES_MEMORY_LIMIT` (and request); OOMKill risk |

Constraints that fall out of the project's "absent ≠ zero" rule:

- `memory-pressure` never fires when `MemoryLimitBytes == 0` — an absent limit
  series is not a denominator.
- `cpu-throttle` prints the current request/limit only when non-zero; otherwise
  it says they are unset, which is itself the finding.
- `long-job` is the only rule that works without `Usage`: duration comes from
  the GitLab job, so it still fires for jobs whose pod never correlated.

### `java-threads` body

Detection regexes over the trace (case-insensitive, first match wins):

- maven — `\bmvn\b`, `\[INFO\] Scanning for projects`, `maven-\w+-plugin`
- gradle — `\bgradlew?\b`, `Welcome to Gradle`, `Starting a Gradle Daemon`
- generic java — `\bjava\s+-`, `openjdk`

The explanation: Maven's `-T 1C` and Gradle's default worker count both size
themselves from the *host* core count, and the JVM only derives
`Runtime.availableProcessors()` from the cgroup quota when a CPU **limit** is
set. With requests only, a JVM on a 64-core node builds 64-wide thread pools
inside a 1-core slice — which is exactly what surfaces as CFS throttling.

Remedies offered:

- `mvn -T 2` (an explicit count, not `1C`)
- `./gradlew --max-workers=2`, or `org.gradle.workers.max=2` in
  `gradle.properties`
- Surefire `forkCount` when tests fork JVMs
- `JAVA_TOOL_OPTIONS=-XX:ActiveProcessorCount=2` — never
  `-XX:-UseContainerSupport`

Links included in the body:

- <https://cwiki.apache.org/confluence/display/MAVEN/Parallel+builds+in+Maven+3>
- <https://docs.gradle.org/current/userguide/command_line_interface.html> —
  documents `--max-workers` ("Sets the maximum number of workers that Gradle may
  use. Default is number of processors.")
- <https://kestra.io/docs/administrator-guide/jvm-cpu-limits>

## 3. Orchestration: `Reporter.Advise`

```go
// ErrJobNotFound is returned when jobFilter matches no job of the pipeline.
var ErrJobNotFound = errors.New("job not found in pipeline")

// Advise builds Facts for every job of the pipeline (or the one matching
// jobFilter, by numeric ID or exact name) and runs the engine over them.
func (r *Reporter) Advise(ctx context.Context, projectID, pipelineID int64,
    jobFilter string, eng *advice.Engine) ([]advice.Advice, error)
```

Per job:

1. Build `Facts` with stage, name and `Duration = FinishedAt.Sub(StartedAt)`.
2. Resolve the pod and query `PodUsage`; a failure leaves `Usage` nil and the
   job still gets duration-based advice. Only the jobs listing failing aborts.
3. If `eng.NeedsTrace(f)`, fetch `GitLab.JobTrace` — **one extra API call, and
   only for jobs a rule asked about**. On a 30-job pipeline with 2 throttled
   jobs that is 2 fetches, not 30.
4. `eng.Analyze(f)`, appended in job order.

Jobs that never ran (zero `StartedAt`/`FinishedAt`) are skipped entirely.

## 4. Surfaces

### Note command

`internal/command`:

- `KindAdvise` added to `Kind`.
- `adviseRE = ^advise(?:\s+(?:job\s+)?(\S+))?$` (case-insensitive), parsed like
  `details`. Jobs only — no pod target, since advice is about a CI job.
- `HelpText` gains: `` `advise` `` / `` `advise <job>` ``.
- `Handler` gains an `Advisor` field, an interface so the handler stays
  stubbable and does not grow a reporter dependency:

  ```go
  type Advisor interface {
      Advise(ctx context.Context, projectID, pipelineID int64, jobFilter string) ([]advice.Advice, error)
  }
  ```

  `serve.go` wires it to a small adapter that closes over the reporter and the
  configured engine.

Authorization is unchanged and shared with `details`: bot-rooted discussion,
valid signed marker matching the MR, marker loop guard.

`ErrJobNotFound` replies ``` `<name>` is not part of pipeline #N's report. ```,
matching the existing `details` wording.

### CLI

`cmd/bot/advise.go`:

```txt
bot advise --project <id> <pipeline-id> [--job <name>]
```

A cobra subcommand next to `run` and `serve` (per CLAUDE.md: new entry points
are subcommands, not flags on root). `--job` accepts a numeric ID or an exact
name, reusing `run`'s `findJob` matcher — which moves to a shared helper in
`cmd/bot/deps.go`. Output is the exact markdown the note reply posts, so the two
surfaces cannot drift.

## 5. Config

Two new variables in `internal/config`, validated at load like the existing
ones:

| Variable | Default | Validation |
| --- | --- | --- |
| `LONG_JOB_DURATION` | `10m` | parses as a duration, `> 0` |
| `MEMORY_PRESSURE_RATIO` | `0.9` | parses as a float in `(0, 1]` |

Both are optional and used by `serve` and `advise` alike. They are exposed in
the Helm chart (`values.yaml` alongside `throttleWarnRatio`, plus the
`deployment.yaml` env block) and documented in `docs/usage.md`.

## 6. Testing

- `internal/advice`: one table-driven test **per rule file**, exercising the
  rule in isolation (no engine) — fires / does not fire / absent-series cases.
- Engine tests: registration-order output, `New` filtering by name, unknown name
  → error, and `NeedsTrace` true only when a `TraceConsumer` says so.
- `Render` golden files: `testdata/advise.md` (every rule firing across two
  jobs) and `testdata/advise-clean.md` (the `You are all good dude!` case).
- `internal/reporter`: a fake GitLab client **counting `JobTrace` calls**,
  proving the trace is fetched only for the throttled job; plus a case where
  pod correlation fails and the job still gets `long-job` advice.
- `internal/command`: `Parse` cases for `advise`, `advise foo`, `advise job
  foo`, and non-matches; a handler test with a stub `Advisor` asserting one
  reply carrying the marker.
- `internal/e2e`: extend the note-command chain with an `advise` note and assert
  the recorded reply body.
- `cmd/bot`: `advise_test.go` in the style of `run_test.go`.

Definition of done: `mise r lint test` clean with the race detector.

## 7. Documentation

- `docs/usage.md`: `advise` rows in the commands table, the two new env vars in
  the config table, and a short "how advice is generated" note.
- `README.md`: `advise` in the command list.

## Out of scope

- **Over-provisioning advice** (usage far below requests). Easy to add later as
  a fifth rule file; not requested.
- **`ADVICE_RULES` env var.** The engine seam supports it; wiring waits for a
  user who needs it.
- Any change to the pipeline report note itself.
