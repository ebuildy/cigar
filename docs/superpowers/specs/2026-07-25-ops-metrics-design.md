# Ops metrics (`cigar_`), PodMonitor & Grafana dashboard

Status: approved 2026-07-25 — implemented; amended 2026-07-26 with
`cigar_log_total`, queue backpressure logging and the operator guide
([`docs/monitoring.md`](../../monitoring.md)).

## Goal

Expose the bot's own operational metrics in Prometheus format (already reserved
`/metrics` on the ops server, `:8081`), ship a Helm `PodMonitor` to scrape them,
and provide a Grafana dashboard. All metrics are prefixed `cigar_`.

## Metrics

All registered on a dedicated `*prometheus.Registry` (isolated from the global
default registry — clean tests, and standard `go_*`/`process_*` collectors can be
added without polluting other packages).

| Metric | Type | Labels | Meaning / increment site |
|---|---|---|---|
| `cigar_webhook_calls_total` | CounterVec | `project`, `status` | +1 per webhook delivery, recorded in a `defer` so the final HTTP status is known; covers pre-parse failures (`401` carries `project="0"`) |
| `cigar_command_calls_total` | CounterVec | `project`, `command` | +1 per executed command; `command` = kind (`help`/`details`/`advise`) |
| `cigar_log_total` | CounterVec | `level`, `name` | +1 per warn-or-worse log entry; `level` = zap level, `name` = logger name (subsystem), `root` when unnamed |
| `cigar_prometheus_query_duration_seconds_total` | Counter | — | accumulated wall-clock seconds spent in Prometheus `Query`/`QueryRange` |
| `cigar_users_active` | Gauge | — | count of distinct GitLab user IDs seen (pipeline triggerer + note author), tracked in an in-memory set; resets on restart |

The `status` label on `cigar_webhook_calls_total` was added during
implementation: without it the counter could not distinguish accepted from
rejected deliveries, which is the whole point of alerting on it. `503` (queue
full ⇒ event dropped) and `401` are the two statuses worth paging on.

## Package: `internal/telemetry`

Single `Metrics` struct owning the registry, collectors, and the distinct-user
set (guarded by a mutex). Typed record methods keep label handling in one place.

```go
func New() *Metrics
func (m *Metrics) Handler() http.Handler                     // promhttp over the registry
func (m *Metrics) RecordWebhook(projectID int64, status int)
func (m *Metrics) RecordCommand(projectID int64, kind string)
func (m *Metrics) ObserveQuery(d time.Duration)
func (m *Metrics) RecordUser(userID int64)                   // adds to set; sets gauge to len
func (m *Metrics) LogOption() zap.Option                     // counting core proxy, see below
```

All record methods are nil-receiver safe, so paths without telemetry (`bot run`)
pass `nil` and pay nothing.

## Log counting (`cigar_log_total`)

A catch-all health signal: every failure the bot logs is counted, including those
with no dedicated metric. SREs alert on the rate and use `name` to route.

Implemented in `internal/telemetry/logcore.go` as a `zapcore.Core` **proxy**
(`countingCore`), installed with `log.WithOptions(m.LogOption())` on the root
logger in `serve` — before any subsystem logger is derived from it.

Three non-obvious decisions:

- **Count in `Check`, not `Write`.** `Check` runs once per log call, ahead of the
  wrapped core's level filtering; `Write` only runs for entries that survived it.
- **`Enabled` returns true for warn-and-above regardless of the wrapped core.**
  This is why zap's built-in `zapcore.RegisterHooks` was rejected: hooks only fire
  for entries that pass the level filter, so `--log-level error` would silently
  pin `{level="warn"}` at zero. The counter must measure what the bot experienced,
  not what the operator chose to print. Entries the wrapped core rejects are
  counted and then discarded by it — nothing extra reaches stdout.
- **`With` must rewrap.** Otherwise `logger.With(...)` (used per pipeline in
  `serve`) returns the bare inner core and stops counting from that point on.

The `name` label required naming the loggers — nothing called `.Named()` before,
so every entry would have landed in a single bucket. Names are applied at the
wiring points in `cmd/bot/deps.go` and `serve.go`: `webhook`, `queue`, `worker`,
`gitlab`, `metrics`, `correlate`, `reporter`, `command`; unnamed ⇒ `root`. This
also adds a `logger` field to the JSON logs. **A new subsystem that reuses its
parent's logger silently reports as `root`** — noted in `CLAUDE.md`.

Only warn and above are counted (`logCountLevel`); info/debug is ordinary traffic
already covered by the webhook counter and would dilute the signal.

## Queue backpressure

The worker queue is the one saturation point with no metric (see *Out of scope*),
so it reports through logs instead — both counted by `cigar_log_total`:

- `queue` logger, **warn** `worker queue nearly full` at ≥80% of the 128-event
  buffer (`queueWarnPercent`, rounded up), with `depth`/`capacity`/`warn_at`.
  **Edge-triggered** via an `atomic.Bool`: one line per near-full episode rather
  than one per event, re-armed when an enqueue observes the queue back below the
  threshold. Racing enqueues may cost one extra line — cheaper than a lock on the
  path the webhook handler blocks on.
- `webhook` logger, **error** `queue full, dropping event` / `dropping note
  command` (was warn): the event is discarded for good, GitLab does not retry, so
  that pipeline never gets a comment. Kept in the handler rather than in
  `Enqueue` because only the handler knows the `pipeline_id`/`note_id`.

`queue` changed from a bare `chan webhook.Event` to a small struct (channel +
logger + threshold) built by `newQueue`.

## Wiring

- **`cmd/bot/serve.go`**: `m := telemetry.New()` **first**, then
  `log := logger.WithOptions(m.LogOption())` before anything else uses the
  logger; serve `m.Handler()` on ops `/metrics` (replaces the existing TODO);
  pass `m` into `webhook.NewApp`, the command handler, and
  `metrics.NewPromSource`, and a `.Named()` logger into each.
- **`internal/webhook`**: handler gets a small `Recorder` interface
  (`RecordWebhook`, `RecordUser`) so it stays ignorant of Prometheus/GitLab
  (telemetry is neither). `PipelineEvent` gains `User.ID` (note payload already
  parses `author_id`). Record webhook call + user right after successful parse.
  Nil recorder ⇒ no-op (tests, defensive).
- **`internal/command`**: `Handler` gains an optional metrics field; record on
  each handled command with its kind. Nil ⇒ no-op.
- **`internal/metrics`**: `PromSource` gains a nil-safe `QueryObserver` interface
  field; `NewPromSource` takes an observer. `serve` passes `m`; `run` passes
  `nil` (it serves no `/metrics`). Time both `s.api.Query` / `QueryRange` sites.

`telemetry.Metrics` satisfies both the webhook `Recorder` and the metrics
`QueryObserver` interfaces (interfaces declared in the consumer packages).

## Helm chart

- `templates/podmonitor.yaml`: `monitoring.coreos.com/v1 PodMonitor`, gated by
  `.Values.podMonitor.enabled` (default `false`, so the chart never hard-requires
  the Prometheus Operator CRD). Scrapes port `ops` at `/metrics`. Configurable
  `interval`, `scrapeTimeout`, extra `labels` (e.g. `release: kube-prometheus`),
  `relabelings`. Selector reuses `cigar.selectorLabels`.
- `values.yaml`: add a `podMonitor` block.
- NetworkPolicy already allows ingress to the ops port — no change.

## Grafana dashboard

- `deploy/grafana/cigar-dashboard.json`: templated `datasource` variable and a
  `project` filter; rows *Adoption* (stats), *Traffic* (webhook call rate by
  project, command call rate by kind), *Runtime* (query time rate, go/process
  basics) and *Health* (warn/error log rate by level and subsystem).

## Documentation

- [`docs/monitoring.md`](../../monitoring.md) is the operator-facing guide:
  endpoints, per-metric semantics **and caveats**, scraping (PodMonitor +
  annotation fallback), useful PromQL, a ready-to-apply `PrometheusRule` with
  alert examples across four groups, and the log signals. Linked from the
  README's Metrics section.
- Caveats it must keep carrying, because they change how the metrics are read:
  `cigar_users_active` is per-process and must be aggregated with `max`, not
  `sum`; the query-duration counter is a sum with no count, so it yields
  saturation but never latency; `200` on a webhook means "handled", not "report
  posted"; `cigar_log_total` counts independently of `--log-level`.

## Testing

- `internal/telemetry`: distinct-user counting; counter/gauge values via
  `prometheus/client_golang/prometheus/testutil`.
- `internal/telemetry/logcore_test.go`: labels by level and name; debug/info not
  counted (asserted via series count); **counted below the configured level**
  (the property that justifies the proxy); `With` survival; nil-`Metrics`
  pass-through.
- `internal/webhook`: fake recorder asserted for project + user on pipeline/note.
- `cmd/bot`: queue warns once per crossing and re-arms only after a real
  recovery; and an end-to-end assertion that scrapes the handler for
  `cigar_log_total{level="warn",name="queue"}` — unit tests alone cannot catch a
  missing `.Named()` at a wiring point.
- `helm lint` + `helm template` render the PodMonitor when enabled and omit it
  when disabled. Dashboard JSON validity checked.
- e2e stays green (all additions are nil-safe / additive).

## Out of scope

- Persisting the active-user set across restarts (adoption gauge is best-effort).
- Per-user or per-command-name cardinality (kept to bounded labels).
- The log **message** as a label on `cigar_log_total`: free-form, so cardinality
  would grow with every new log call site. Detect on the counter, diagnose in the
  logs — the line carries the IDs.
- **Queue depth as a metric** (considered 2026-07-26, rejected). A `GaugeFunc`
  over `len(ch)` is trivial, but a 30s scrape samples a queue that drains in
  seconds as empty almost every time: a burst filling 100 of 128 slots between
  two scrapes is invisible, and you would see `0, 0, 0` then a `503`. Doing it
  properly means a high-water mark reset at collection, or a queue-wait
  histogram — deferred as not worth the complexity. The 80% warning covers the
  need from inside the enqueue path, where it cannot miss a burst.
