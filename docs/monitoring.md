# Monitoring

cigar exposes its **own** operational metrics — how many webhooks it received,
which commands users ran, how much time it spends querying Prometheus — so you
can tell whether the bot is healthy and whether anyone is using it.

> Not to be confused with the *pipeline* metrics the bot reports in merge-request
> comments (CPU, memory, throttling of runner pods). Those are read from your
> cluster's Prometheus and never re-exported here.

---

## Endpoints

The `serve` command starts a second HTTP server (the **ops** server) on
`server.ops_addr` / `SERVER_OPS_ADDR`, default `:8081`:

| Path | Purpose |
| --- | --- |
| `/metrics` | Prometheus text format, all bot metrics |
| `/healthz` | liveness — `200` as soon as the process is up |
| `/readyz` | readiness — `200` as soon as the process is up |

The webhook itself listens separately on `:8080`. Nothing on `:8081` is
authenticated, so do **not** expose the ops port through your ingress — the
chart's Service exposes it as the `ops` port for in-cluster scraping only, and
the NetworkPolicy limits ingress to those two ports.

`bot run` (the one-shot CLI) exposes no metrics at all; telemetry is nil there
and every record call is a no-op.

---

## Metrics reference

All bot metrics are prefixed `cigar_` and live on a dedicated registry
(`internal/telemetry`), alongside the standard `go_*` and `process_*` collectors.

### `cigar_webhook_calls_total`

**Counter**, labels `project`, `status`.

One increment per webhook delivery to `POST /webhook`, tagged with the HTTP
status cigar returned. This is the main traffic and error signal.

| `status` | Meaning |
| --- | --- |
| `200` | accepted **or** deliberately ignored (non-terminal pipeline status, non-`Pipeline`/`Note` event, note that isn't a command, MR-less payload) |
| `400` | payload failed to parse — malformed JSON from GitLab, or something else posting to the endpoint |
| `401` | no configured auth method accepted the request (wrong `X-Gitlab-Token`, bad signature, or an expired signature timestamp) |
| `413` | body over the 1 MiB limit (Fiber `BodyLimit`) |
| `503` | internal queue full — **the event was dropped**, no comment will be posted |

Caveats:

- `project="0"` means the project ID wasn't known yet — every `401` lands there,
  since authentication happens before the body is parsed.
- `200` is *not* the same as "a report was posted". Most deliveries are ignored
  by design (running/pending pipelines, pushes with no MR). There is no metric
  for "note upserted"; use the logs (`report posted`) for that.

### `cigar_command_calls_total`

**Counter**, labels `project`, `command`.

One increment per interactive command executed from an MR note reply. `command`
is the verb: `help`, `details`, `advise` (or `unknown`). Only incremented when
`commands.enabled` is on. This is the adoption signal that matters most — it
counts humans deliberately asking the bot for something.

### `cigar_prometheus_query_duration_seconds_total`

**Counter**, no labels.

Cumulative wall-clock seconds spent inside Prometheus `Query` / `QueryRange`
calls. It is a *sum only* — there is no companion count or histogram, so you
cannot derive a per-query latency from it. What you can derive is **saturation**:

```promql
sum by (pod) (rate(cigar_prometheus_query_duration_seconds_total[5m]))
```

Since one replica processes events with a single worker goroutine, this value is
roughly "fraction of wall-clock time this replica spends waiting on Prometheus".
Approaching `1` means the worker is fully busy querying and the queue will start
backing up (watch for `status="503"` next).

### `cigar_users_active`

**Gauge**, no labels.

Count of **distinct GitLab user IDs seen since process start** — pipeline
triggerers and note authors. Deliberately simple, with the consequences that
implies:

- It is per-process and **in-memory**: it resets to 0 on every restart, and each
  replica counts its own set.
- It only ever goes up within a process. It is *not* "users active in the last
  24h" — no time window, no decay.
- With multiple replicas the sets overlap, so **aggregate with `max`, never
  `sum`** (`sum` double-counts users whose events hit both pods):

  ```promql
  max(cigar_users_active{job=~".*cigar.*"})
  ```

Treat it as a coarse adoption trend, not an accounting figure.

### `cigar_log_total`

**Counter**, labels `level`, `name`.

One increment per **warn-or-worse** log entry. `level` is `warn`, `error`,
`dpanic`, `panic` or `fatal`; `name` is the zap logger name of the subsystem
that logged it:

| `name` | Subsystem |
| --- | --- |
| `webhook` | HTTP handler: auth failures, malformed payloads, dropped events |
| `queue` | worker queue nearly full |
| `worker` | pipeline/note processing failures |
| `reporter` | report build: per-job pod resolution and metric errors |
| `gitlab` | GitLab API client |
| `metrics` | Prometheus queries |
| `correlate` | job → pod correlation |
| `command` | interactive command handling |
| `root` | anything logged by the unnamed root logger (startup, shutdown) |

This is the catch-all health signal: anything the bot considers abnormal shows up
here, including failures that have no dedicated metric. Alert on the *rate*, and
use `name` to route — a spike in `gitlab` is a token or upstream problem, a spike
in `queue`/`worker` is a capacity problem.

Two design notes worth knowing:

- **It counts regardless of the configured log level.** `cigar_log_total` is fed
  by a `zapcore.Core` proxy that counts the entry *before* delegating to the real
  core, and reports itself enabled for warn and above. Running with
  `--log-level error` still increments `{level="warn"}` — you just won't see the
  lines in stdout. The metric measures what the bot experienced, not what you
  chose to print.
- **The log message is deliberately not a label.** Messages are free-form and
  would make cardinality grow with every new log call site. Use the counter to
  detect, then the logs to diagnose — the log line carries the IDs.

### Runtime metrics

`go_goroutines`, `go_memstats_*`, `process_resident_memory_bytes`,
`process_cpu_seconds_total`, `process_start_time_seconds`, … — the standard
collectors. `process_start_time_seconds` is what the restart alert below keys on.

### Cardinality note

`project` is unbounded: one series per GitLab project that ever fires a webhook
(times the statuses seen). That's fine for tens or hundreds of projects. If you
run cigar in front of a very large instance, drop the label at scrape time:

```yaml
podMonitor:
  metricRelabelings:
    - action: labeldrop
      regex: project
```

You keep the totals and error rates, you lose the per-project breakdown.

---

## Scraping

### PodMonitor (Prometheus Operator)

The chart ships one, disabled by default so it never hard-requires the CRD:

```sh
helm upgrade --install cigar deploy/chart/cigar \
  --set podMonitor.enabled=true \
  --set podMonitor.labels.release=kube-prometheus-stack   # your operator's selector
```

Other knobs: `podMonitor.namespace`, `interval` (default `30s`),
`scrapeTimeout` (`10s`), `relabelings`, `metricRelabelings`. See
[`deploy/chart/cigar/templates/podmonitor.yaml`](../deploy/chart/cigar/templates/podmonitor.yaml).

With the operator's defaults the `job` label becomes `<namespace>/<podmonitor-name>`.
The queries below use `job=~".*cigar.*"` so they work either way — adapt them if
you set `jobLabel` or renamed the release.

### Without the operator

Annotate the pods and let a scrape-config pick them up:

```yaml
podAnnotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "8081"
  prometheus.io/path: /metrics
```

### Checking by hand

```sh
kubectl port-forward deploy/cigar 8081:8081
curl -s localhost:8081/metrics | grep '^cigar_'
```

---

## Useful queries

```promql
# Webhook rate by project and status
sum by (project, status) (rate(cigar_webhook_calls_total[5m]))

# Error ratio (everything that isn't 200)
sum(rate(cigar_webhook_calls_total{status!="200"}[15m]))
  / sum(rate(cigar_webhook_calls_total[15m]))

# Dropped events (queue full) over the last hour
sum(increase(cigar_webhook_calls_total{status="503"}[1h]))

# Command mix
sum by (command) (rate(cigar_command_calls_total[1h]))

# Top projects by command usage over a week
topk(10, sum by (project) (increase(cigar_command_calls_total[7d])))

# Query saturation per replica (≈ fraction of time spent in Prometheus)
sum by (pod) (rate(cigar_prometheus_query_duration_seconds_total[5m]))

# Adoption
max(cigar_users_active{job=~".*cigar.*"})

# Where the errors are coming from
sum by (name) (rate(cigar_log_total{level="error"}[15m]))

# Warn/error log rate, all subsystems
sum by (level) (rate(cigar_log_total[5m]))
```

---

## Grafana dashboard

A ready-made dashboard is at
[`deploy/grafana/cigar-dashboard.json`](../deploy/grafana/cigar-dashboard.json):
adoption stats, webhook rate by project and status, command mix, query-time rate,
and go/process basics. Import it and pick your Prometheus data source — it has a
templated `datasource` variable and a `project` filter.

---

## Alerting examples

Drop this in as a `PrometheusRule` (Prometheus Operator) or paste the `groups:`
block into a plain `rule_files:` target. Thresholds are starting points — tune
`for:` durations to your webhook volume, since a low-traffic instance makes
rate-based alerts noisy.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: cigar
  labels:
    release: kube-prometheus-stack   # your operator's selector
spec:
  groups:
    - name: cigar.availability
      rules:
        - alert: CigarDown
          expr: up{job=~".*cigar.*"} == 0
          for: 5m
          labels: { severity: warning }
          annotations:
            summary: "cigar replica {{ $labels.pod }} is not scrapeable"
            description: "Prometheus cannot scrape the ops port. Pipeline reports may be degraded."

        - alert: CigarAllReplicasDown
          expr: sum(up{job=~".*cigar.*"}) == 0
          for: 2m
          labels: { severity: critical }
          annotations:
            summary: "cigar is completely down"
            description: "No replica is up: GitLab webhook deliveries are failing and no MR reports are posted."

        - alert: CigarCrashLooping
          # process_start_time_seconds changes on every (re)start
          expr: changes(process_start_time_seconds{job=~".*cigar.*"}[1h]) > 3
          for: 10m
          labels: { severity: warning }
          annotations:
            summary: "cigar restarted {{ $value }} times in the last hour"
            description: "Check the pod logs — likely a config or GitLab/Prometheus connectivity failure at startup."

    - name: cigar.webhook
      rules:
        - alert: CigarWebhookAuthFailures
          # A misconfigured hook token, or someone probing the endpoint.
          expr: sum(rate(cigar_webhook_calls_total{status="401"}[10m])) > 0
          for: 15m
          labels: { severity: warning }
          annotations:
            summary: "cigar is rejecting webhooks with 401"
            description: >-
              Sustained authentication failures. Check WEBHOOK_SECRET /
              WEBHOOK_SIGNING_TOKEN against the hook configured in GitLab; the
              logs name the offending project ("webhook authentication failed").

        - alert: CigarEventsDropped
          # 503 = the internal queue was full: that pipeline gets no report.
          expr: sum(increase(cigar_webhook_calls_total{status="503"}[15m])) > 0
          labels: { severity: critical }
          annotations:
            summary: "cigar dropped {{ $value | printf \"%.0f\" }} webhook events"
            description: "The worker cannot keep up. Scale up replicas, or check Prometheus query latency (cigar_prometheus_query_duration_seconds_total)."

        - alert: CigarWebhookErrorRatio
          expr: >-
            sum(rate(cigar_webhook_calls_total{status!~"200|401"}[15m]))
              / sum(rate(cigar_webhook_calls_total[15m])) > 0.05
          for: 15m
          labels: { severity: warning }
          annotations:
            summary: "More than 5% of webhook deliveries fail"
            description: "Mostly 400 (malformed payloads) or 413 (bodies over 1 MiB). Inspect the status label breakdown."

        - alert: CigarNoWebhookTraffic
          # Silence-detector: GitLab stopped delivering (hook disabled, ingress
          # broken, network policy). Only meaningful on a busy instance —
          # raise the window or drop this rule if CI is idle at night.
          expr: sum(increase(cigar_webhook_calls_total[2h])) < 1
          for: 30m
          labels: { severity: warning }
          annotations:
            summary: "cigar received no webhook in 2 hours"
            description: "Check the hook status in GitLab (Settings → Webhooks — GitLab disables hooks that keep failing) and the ingress."

    - name: cigar.logs
      rules:
        - alert: CigarErrorLogRate
          # Catch-all: anything the bot logs as an error, including failures
          # with no dedicated metric. `name` says which subsystem.
          expr: sum by (name) (rate(cigar_log_total{level="error"}[15m])) > 0.05
          for: 15m
          labels: { severity: warning }
          annotations:
            summary: "cigar {{ $labels.name }} is logging errors ({{ $value | printf \"%.2f\" }}/s)"
            description: "Grep the pod logs for that logger name. gitlab → token/upstream; metrics → Prometheus; queue/worker → capacity."

        - alert: CigarErrorLogSpike
          # A sudden jump against the last hour, for failures too brief to
          # trip the sustained-rate alert above.
          expr: >-
            sum(rate(cigar_log_total{level="error"}[5m]))
              > 5 * sum(rate(cigar_log_total{level="error"}[1h])) + 0.1
          for: 10m
          labels: { severity: warning }
          annotations:
            summary: "cigar error log rate jumped 5x over the last hour's baseline"

    - name: cigar.saturation
      rules:
        - alert: CigarPrometheusQuerySaturation
          # ≈ fraction of wall-clock time the single worker spends querying.
          expr: sum by (pod) (rate(cigar_prometheus_query_duration_seconds_total[10m])) > 0.8
          for: 15m
          labels: { severity: warning }
          annotations:
            summary: "cigar replica {{ $labels.pod }} is saturated by Prometheus queries"
            description: "The worker is almost always waiting on Prometheus; the queue will start dropping events. Check Prometheus load, or shrink the query windows."

        - alert: CigarGoroutineLeak
          expr: go_goroutines{job=~".*cigar.*"} > 500
          for: 30m
          labels: { severity: warning }
          annotations:
            summary: "cigar goroutine count is {{ $value }}"
            description: "Steady-state should be a few dozen. A sustained climb means leaked goroutines — capture /debug or a goroutine dump before restarting."

        - alert: CigarMemoryNearLimit
          expr: >-
            container_memory_working_set_bytes{container="cigar"}
              / kube_pod_container_resource_limits{container="cigar", resource="memory"} > 0.9
          for: 10m
          labels: { severity: warning }
          annotations:
            summary: "cigar is at {{ $value | humanizePercentage }} of its memory limit"
            description: "OOMKill risk — raise resources.limits.memory in the chart."
```

## Log signals

Every warn-or-worse line is already counted in
[`cigar_log_total`](#cigar_log_total), so you can alert on these without a log
pipeline — but the log line is where the identifying detail lives (the metric
carries only `level` and `name`). cigar logs structured JSON to stdout; the two
messages below are the ones worth knowing by name:

| Level | Logger | Message | Meaning |
| --- | --- | --- | --- |
| `warn` | `queue` | `worker queue nearly full` | the queue crossed 80% of its 128-event buffer (`depth`, `capacity`, `warn_at` fields). The worker is falling behind — drops are close. |
| `error` | `webhook` | `queue full, dropping event` / `queue full, dropping note command` | the event was **discarded**. GitLab does not retry, so that pipeline never gets a comment. Carries `pipeline_id`/`note_id` + `project_id`. |

The queue depth itself is the one thing with no metric: a 30s scrape would sample
it as empty almost every time, since it drains in seconds. The warning is the
substitute — it fires from inside the enqueue path, so it cannot miss a burst
the way a sampled gauge would.

The warning is edge-triggered: one line per near-full episode, not one per event,
so a sustained burst won't flood the log. It re-arms once an enqueue sees the
queue back under the threshold.

The `error` line is the log-side twin of `cigar_webhook_calls_total{status="503"}`
and of the `CigarEventsDropped` alert above — but it names the pipeline that was
lost, which the metric cannot.

### Choosing what to page on

Only two of these deserve to wake someone: `CigarAllReplicasDown` and
`CigarEventsDropped` (a dropped event is a report a developer will never see, and
it is not retried). Everything else is a ticket — cigar is an advisory bot, and a
missing comment doesn't break anyone's pipeline.

---

## Related

- [`docs/usage.md`](usage.md) — deployment, GitLab configuration, interactive commands.
- [`README.md`](../README.md#metrics) — the short version of this reference.
