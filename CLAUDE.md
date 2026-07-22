# CLAUDE.md

## Project: gitlab-ci-resources-bot

A Go service that receives GitLab **Pipeline events** webhooks, queries **Prometheus** (cadvisor metrics) for the resource usage of the Kubernetes runner pods that executed the pipeline's jobs, and posts a **merge request comment** summarizing CPU, memory, throttling, and network usage — with actionable advice.

## What the bot does

1. GitLab fires a Pipeline event webhook when a pipeline reaches a terminal state (`success`, `failed`).
2. The bot validates the webhook, ignores non-terminal statuses and pipelines with no associated MR.
3. For each job in the pipeline, it resolves the runner pod (by pod-name labels the runner injects, see "Pod ↔ job correlation") and queries Prometheus over the job's time window.
4. It aggregates per-job and pipeline-total metrics, then creates or **updates** a single MR note (idempotent — never spam one comment per pipeline run).

### Report content

- Pipeline totals: total memory (sum of job peaks), peak memory (max working set), CPU time consumed, network RX/TX.
- Per-job table: job name | CPU time | peak memory | memory request/limit | CPU request/limit | throttled % | network.
- ⚠️ CPU throttling warning when `throttled_periods / periods > threshold` (default 25%), with advice: set `KUBERNETES_CPU_REQUEST` / `KUBERNETES_CPU_LIMIT` GitLab CI variables (and the memory equivalents `KUBERNETES_MEMORY_REQUEST` / `KUBERNETES_MEMORY_LIMIT`) on the job or project.
- Advice when usage ≪ requests (over-provisioned) or peak memory near limit (OOM risk).

## Architecture

```txt
cmd/
  bot/                   # cobra CLI: main.go (root cmd), serve.go (`bot serve`), run.go (`bot run`), deps.go (shared wiring)
internal/
  webhook/               # Fiber app: token check, payload parse, event filter, body limit
  reporter/              # orchestration shared by serve worker + run CLI: jobs -> pods -> usage -> report.Data
  gitlab/                # GitLab API client: jobs list, MR lookup, notes create/update
  metrics/               # Prometheus client + PromQL queries, per-job aggregation
  correlate/             # map GitLab job -> k8s pod/containers (labels/annotations)
  report/                # markdown rendering (templates), advice engine, thresholds
  config/                # env-based config, validation
  e2e/                   # end-to-end test: real components against mock GitLab + Prometheus HTTP servers
```

- CLI via `spf13/cobra`: `bot serve` runs the service (container CMD); `bot run --project <id> <pipeline-id>` builds the same report once and prints it to stdout (JSON logs also go to stdout; `--log-level error` keeps the output to just the report). New entry points are subcommands, not flags on root.
- Both paths go through `reporter.Reporter.Build` → `report.Render` — never fork the report logic per entry point. Per-job failures (no pod, metrics error) leave that job's `Usage` nil; only the GitLab jobs listing failing aborts a report.
- HTTP via `gofiber/fiber/v3`: `webhook.NewApp` builds the webhook Fiber app (routes, body limit); a second Fiber app serves ops endpoints on `:8081`.
- Keep packages under `internal/`; no public API surface.
- `webhook` must not know about Prometheus or GitLab clients — it enqueues work; a worker processes it. Webhook handler returns `200` fast (GitLab timeout is 10s; metric queries can be slow).
- Use interfaces at package boundaries (`metrics.Source`, `gitlab.Client`) so tests can stub them.

## Documentation

All project documentation lives under `docs/` — never add stray Markdown docs at
the repo root. The only permitted root-level docs are `README.md` (repo landing
page) and this `CLAUDE.md`; everything else (usage guides, design specs,
implementation plans, runbooks, ADRs) goes in `docs/` (e.g. `docs/usage.md`,
`docs/superpowers/specs/`, `docs/superpowers/plans/`). When adding a new doc,
create it in `docs/` and link it from `README.md` if it's user-facing.

## Key implementation notes

### Webhook security (non-negotiable)

- Authenticate via `AUTH_METHODS` (ordered, comma-separated: `secret`, `signature`; default `secret`). `secret`: constant-time compare of `X-Gitlab-Token` against `WEBHOOK_SECRET` (an empty configured secret never authenticates). `signature`: GitLab signing token — verify the `webhook-signature` HMAC-SHA256 over `{webhook-id}.{webhook-timestamp}.{body}` using `WEBHOOK_SIGNING_TOKEN` (whsec_), rejecting timestamps outside a 5m window (replay protection). Methods are tried in order; the first that authenticates the request wins; none → `401`, no body detail.
- Only accept `X-Gitlab-Event: Pipeline Hook`; ignore everything else with `200` (so GitLab doesn't disable the hook).
- Enforce a max request body size (1 MiB via Fiber `BodyLimit` → `413`) and read/write timeouts.
- Serve HTTPS via ingress/TLS termination; the pod listens plain HTTP on `:8080`, metrics/health on `:8081` (`/healthz`, `/readyz`, `/metrics`).
- GitLab API token: use a project/group access token with `api` scope, least privilege, injected via Secret. Never log tokens or full payloads at info level.
- Rate-limit per project ID; dedupe retried webhook deliveries by `pipeline.id` + `status`.

### Pod ↔ job correlation

The pod-correlation strategy is selected by `POD_RESOLVER` (default `trace`):

- `trace` (default): fetch the job's GitLab trace and parse the runner's `Running on <pod> via <manager>` line (first match wins, ANSI-tolerant). For the Kubernetes executor `<pod>` is the build pod's hostname, matching the cadvisor `pod` label. No pod-label scraping required. See `internal/correlate/trace.go`.
- `prometheus`: join `kube_pod_labels{label_job_id="<id>"}` to find the pod name, then filter cadvisor series by `pod`. Requires the runner to inject a `job_id` pod label that kube-state-metrics exposes. See `internal/correlate/prom.go`.

Either way, always exclude the `POD`/pause container (`container!="", container!="POD"`), and pod name follows the pattern `runner-<token>-project-<id>-concurrent-<n>`.

### PromQL queries (per job, over `[started_at, finished_at]`)

- Peak memory: `max_over_time(container_memory_working_set_bytes{pod="..."}[<window>])` per container, summed.
- CPU time: `increase(container_cpu_usage_seconds_total{...}[<window>])` → render as millicore-seconds or "232m avg".
- Throttling: `increase(container_cpu_cfs_throttled_periods_total[...]) / increase(container_cpu_cfs_periods_total[...])` per container.
- Network: `increase(container_network_receive_bytes_total{...}[<window>])` and transmit equivalent (pod-level, no `container` label).
- Requests/limits: `kube_pod_container_resource_requests` / `kube_pod_container_resource_limits` (kube-state-metrics).
- Account for Prometheus scrape interval (`SCRAPE_INTERVAL`, default `30s`): pad windows by one scrape interval; short jobs (<2 scrapes) get a "low confidence" marker, not fabricated numbers.
- Absent series ≠ zero: a query matching nothing leaves the field unset, it is never written as `0` measured.

### GitLab API

- Use `gitlab.com/gitlab-org/api/client-go`.
- Find MR via pipeline payload (`merge_request` field) or `GET /projects/:id/merge_requests?pipeline_id=`; if none → skip silently.
- Idempotent comment: search existing MR notes for the HTML marker `<!-- ci-resources-bot -->`; update that note instead of creating a new one.

### Config (env only, 12-factor)

`AUTH_METHODS` (default `secret`), `WEBHOOK_SECRET`, `WEBHOOK_SIGNING_TOKEN`, `GITLAB_URL`, `GITLAB_TOKEN`, `PROMETHEUS_URL`, `POD_RESOLVER` (default `trace`; `trace` parses the job's GitLab trace for the `Running on <pod> via …` line, `prometheus` joins `kube_pod_labels{label_job_id}`), `THROTTLE_WARN_RATIO` (default `0.25`), `SCRAPE_INTERVAL` (default `30s`), `LISTEN_ADDR`, `LOG_LEVEL` (default `info`; also settable via the `--log-level` root flag, which takes precedence). Fail fast at startup on missing required vars. `WEBHOOK_SECRET` is required only when `secret` is an enabled auth method; `WEBHOOK_SIGNING_TOKEN` is required only when `signature` is an enabled auth method. Both are required by `serve` only — `bot run` works without either.

## Go conventions

- Go ≥ 1.26, modules; `go.mod` module path `gitlab.com/<group>/gitlab-ci-resources-bot`.
- Standard library first; approved deps: `gofiber/fiber/v3` (HTTP server), `spf13/cobra` (CLI), `client-go` (GitLab), `prometheus/client_golang` (API + own metrics), `go.uber.org/zap` for logging (structured JSON to stdout).
- Logging: `go.uber.org/zap`, JSON encoding, written to **stdout**. The level is a persistent root flag `--log-level` (`debug`/`info`/`warn`/`error`), defaulting to `$LOG_LEVEL` then `info`; the root command builds the logger in `PersistentPreRunE` (`cmd/bot/deps.go:newLogger`) and installs it via `zap.ReplaceGlobals`. Pass `*zap.Logger` down through constructors; use typed fields (`zap.String`, `zap.Int64`, `zap.Error`), never loose key/value pairs. Tests use `zap.NewNop()`.
- Errors: wrap with `fmt.Errorf("...: %w", err)`; no `panic` outside `main`.
- Context everywhere: every outbound call takes `context.Context` with timeout.
- Report rendering via `text/template` with golden-file tests (`testdata/*.md`).
- Table-driven tests; webhook handler tested via Fiber's `app.Test` (body-limit `413` needs a real listener — fasthttp rejects below `app.Test`'s reach); fake Prometheus/GitLab via interfaces. Target: `internal/report` and `internal/webhook` fully covered.
- `internal/e2e` runs the whole chain (webhook app → queue → worker → GitLab/Prometheus clients) against `httptest` mock servers: mock GitLab serves jobs/notes REST endpoints and records note upserts; mock Prometheus serves `/api/v1/query` and records PromQL. Extend it when adding pipeline behavior — it is the proof that note idempotency and pod-filtered queries survive refactors.

## Commands

Use mise:

```sh
mise r build          # go build ./cmd/bot
mise r test           # go test -race ./... (includes e2e)
mise r test:e2e       # only internal/e2e, verbose, cache-busted
mise r lint           # golangci-lint run
mise r docker         # multi-stage build, distroless/static final image, nonroot
mise r release:snapshot  # local goreleaser snapshot (no publish), artifacts in dist/
```

CI is GitHub Actions (`.github/workflows/`), running the mise tasks via `jdx/mise-action` so CI and local use the same pinned toolchain:

- `ci.yml` — on push to main and PRs: lint → test (race, incl. e2e) → build.
- `release.yml` — on `v*` tags: `goreleaser release` publishes binaries (linux/darwin × amd64/arm64), archives, checksums and changelog as a GitHub release. Config in `.goreleaser.yaml`; the version is stamped into `main.version` (`bot --version`).

## Deployment

Helm chart in `deploy/chart/cigar`: Deployment (2 replicas, PDB), Service (http 8080 + ops 8081), Ingress (TLS), NetworkPolicy (ingress limited to the two ports; egress only DNS + GitLab + Prometheus), resources set (practice what we preach), `runAsNonRoot` uid 65532, read-only rootfs, seccomp `RuntimeDefault`, no SA token automount. Env vars map to `config.*` values; `WEBHOOK_SECRET`/`WEBHOOK_SIGNING_TOKEN`/`GITLAB_TOKEN` come from `secrets.existingSecret` (or a chart-managed Secret). Image: `ghcr.io/ebuildy/cigar`, tag defaults to `appVersion`. Validate chart changes with `helm lint` + `helm template` (IDE YAML errors on Go template syntax are noise).

## Definition of done for changes

- `mise r lint test` clean, race detector on.
- New PromQL queries are verified against a real Prometheus snapshot in `testdata/` — never merge queries verified only "by eye".
- Webhook handler changes require a test proving invalid/missing token → 401 and oversized body → 413.
- Any change to the comment format updates the golden files and the README screenshot.