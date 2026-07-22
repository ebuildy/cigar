# CIgar

<!-- markdownlint-disable MD033 -->
<p align="center">
  <img src="doc/logo.svg" alt="CIgar logo" width="200">
</p>
<!-- markdownlint-enable MD033 -->

A Go service that turns your CI pipelines' **actual resource usage** into merge request feedback.

It receives GitLab **Pipeline event** webhooks, queries **Prometheus** (cadvisor metrics) for the CPU, memory, throttling and network usage of the Kubernetes runner pods that executed the pipeline's jobs, and posts a **single, continuously-updated MR comment** with the numbers and actionable advice.

[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/ebuildy/cigar/badge)](https://scorecard.dev/viewer/?uri=github.com/ebuildy/cigar)

## What you get on your MR

- **Pipeline totals** — total memory (sum of job peaks), peak memory, CPU time consumed, network RX/TX.
- **Per-job table** — job name, CPU time, peak memory, memory request/limit, CPU request/limit, throttled %, network.
- **⚠️ CPU throttling warnings** when `throttled_periods / periods` exceeds the threshold (default 25 %), with concrete advice: set `KUBERNETES_CPU_REQUEST` / `KUBERNETES_CPU_LIMIT` (and the memory equivalents) on the job or project.
- **Right-sizing hints** — over-provisioning advice when usage ≪ requests, OOM-risk warning when peak memory is near the limit.

The comment is idempotent: the bot finds its previous note (via an HTML marker) and updates it in place — one comment per MR, never spam.

## How it works

```text
GitLab ──Pipeline Hook──▶ webhook handler ──▶ queue ──▶ worker
                          (validate, 200 fast)          │
                                                        ├─▶ GitLab API: jobs, MR
                                                        ├─▶ correlate job → runner pod
                                                        ├─▶ Prometheus: usage over job window
                                                        └─▶ render report, upsert MR note
```

1. GitLab fires a webhook when a pipeline reaches a terminal state (`success`, `failed`).
2. The handler validates the token, ignores non-terminal statuses and pipelines without an MR, and enqueues the event (it answers within GitLab's 10 s timeout; slow metric queries happen in the worker).
3. For each job, the worker resolves the runner pod (pod labels, with a pod-name-pattern fallback) and queries Prometheus over the job's `started_at`–`finished_at` window, padded by one scrape interval. Jobs shorter than two scrapes are flagged "low confidence" rather than given fabricated numbers.
4. Metrics are aggregated per job and pipeline, rendered to markdown, and posted as the MR note.

## Project layout

```text
cmd/bot/            cobra CLI: `bot serve` (webhook service), `bot run` (one-shot report to stdout)
internal/
  webhook/          Fiber app: token check, payload parse, event filter, body limit, enqueue
  reporter/         orchestration shared by serve and run: jobs → pods → usage → report data
  gitlab/           GitLab API client: jobs list, MR lookup, notes create/update
  metrics/          Prometheus client + PromQL queries, per-job aggregation
  correlate/        map GitLab job → k8s pod/containers
  report/           markdown rendering (text/template), advice engine, thresholds
  config/           env-based config, fail-fast validation
  e2e/              end-to-end test: real components against mock GitLab + Prometheus servers
```

Packages communicate through interfaces (`metrics.Source`, `gitlab.Client`, `correlate.Resolver`) so tests can stub every boundary.

The binary is a [Cobra](https://github.com/spf13/cobra) CLI, and HTTP is served by [Fiber v3](https://github.com/gofiber/fiber): one app for the webhook on `:8080`, one for ops endpoints on `:8081`.

```sh
bot serve                          # run the webhook service
bot run --project 7 12345         # build the report for pipeline 12345 of project 7
                                   # and print it to stdout (add --log-level error
                                   # to keep the output to just the report)
```

`bot run` goes through the exact same report pipeline as the webhook path — only the destination differs (stdout instead of an MR comment). Handy for testing the report against a real pipeline without firing a webhook; it does not need `WEBHOOK_SECRET`.

## Configuration

Environment variables only (12-factor). The bot fails fast at startup if a required variable is missing.

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `AUTH_METHODS` | | `secret` | Ordered, comma-separated webhook auth methods: `secret`, `signature`. First method that authenticates wins |
| `WEBHOOK_SECRET` | ✅ (`serve` only, if `secret` enabled) | — | Value GitLab sends in `X-Gitlab-Token`; compared in constant time |
| `WEBHOOK_SIGNING_TOKEN` | ✅ (`serve` only, if `signature` enabled) | — | GitLab signing token (`whsec_...`); verifies the HMAC-SHA256 `webhook-signature` header |
| `GITLAB_TOKEN` | ✅ | — | Project/group access token, `api` scope, least privilege |
| `PROMETHEUS_URL` | ✅ | — | Prometheus base URL (cadvisor + kube-state-metrics scraped) |
| `POD_RESOLVER` | | `trace` | Pod-correlation strategy: `trace` (parse the job's GitLab trace `Running on <pod> via …` line) or `prometheus` (join `kube_pod_labels{label_job_id}`) |
| `GITLAB_URL` | | `https://gitlab.com` | GitLab instance base URL |
| `THROTTLE_WARN_RATIO` | | `0.25` | Throttled-periods ratio above which a job gets a ⚠️ warning |
| `SCRAPE_INTERVAL` | | `30s` | Prometheus scrape interval; query windows are padded by one interval |
| `LISTEN_ADDR` | | `:8080` | Webhook listen address |
| `OPS_ADDR` | | `:8081` | Health (`/healthz`, `/readyz`) and metrics (`/metrics`) address |
| `LOG_LEVEL` | | `info` | `debug`, `info`, `warn`, `error` — JSON logs to stdout via [zap](https://github.com/uber-go/zap); also settable per-invocation with the `--log-level` flag |

### Migrating to signing-token auth

To move off the legacy secret token: set `AUTH_METHODS=secret,signature` with both credentials configured, migrate your GitLab webhooks to the signing token, then switch to `AUTH_METHODS=signature`.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full workflow (mise setup, tasks, definition of done, releasing).

Toolchain is managed by [mise](https://mise.jdx.dev) (Go 1.26, golangci-lint, goreleaser — pinned in `mise.toml`):

```sh
mise install          # install pinned toolchain
mise r build          # go build ./cmd/bot → bin/bot
mise r test           # go test -race ./... (includes the e2e tests)
mise r test:e2e       # only the e2e tests, verbose (mock GitLab + Prometheus)
mise r lint           # golangci-lint run
mise r run            # go run ./cmd/bot serve (export the env vars above first)
mise r docker         # multi-stage build, distroless/static, nonroot
mise r release:snapshot  # local goreleaser snapshot build (no publish)
```

### CI & releases

GitHub Actions runs the same mise tasks as local development (`.github/workflows/ci.yml`: lint → test → build on every push and PR).

Releases are handled by [GoReleaser](https://goreleaser.com): push a `v*` tag and `.github/workflows/release.yml` publishes a GitHub release with binaries for linux/darwin (amd64/arm64), archives, checksums and a changelog. The tag version is stamped into the binary (`bot --version`).

### Definition of done

- `mise r lint` and `mise r test` clean, race detector on.
- New PromQL queries verified against a real Prometheus snapshot in `testdata/` — never "by eye".
- Webhook handler changes ship with tests proving invalid/missing token → `401` and oversized body → `413`.
- Comment format changes update the golden files (`internal/report/testdata/*.md`) and the README screenshot.

## Security

- Webhook auth via `AUTH_METHODS` (default `secret`): `X-Gitlab-Token` validated with `subtle.ConstantTimeCompare`, and/or the GitLab signing token's HMAC-SHA256 `webhook-signature` header (5-minute replay window). Methods are tried in order; none authenticating gets a bare `401`.
- Only `X-Gitlab-Event: Pipeline Hook` is processed; other events get `200` so GitLab doesn't disable the hook.
- Request bodies capped at 1 MiB via Fiber's `BodyLimit` (`413` beyond that); server read/write timeouts set.
- TLS terminates at the ingress; the pod listens plain HTTP on `:8080`, ops on `:8081`.
- Tokens are injected via Secret and never logged; payloads are not logged at info level.

## Deployment

Helm chart in `deploy/chart/cigar`: Deployment (2 replicas, PDB), Service, Ingress (TLS), NetworkPolicy (egress restricted to DNS, GitLab and Prometheus), resource requests/limits set (practice what we preach), `runAsNonRoot` (distroless uid 65532), read-only rootfs, seccomp `RuntimeDefault`.

```sh
helm install cigar deploy/chart/cigar \
  --set config.prometheusUrl=http://prometheus-operated.monitoring.svc:9090 \
  --set secrets.existingSecret=cigar   # Secret with keys WEBHOOK_SECRET + WEBHOOK_SIGNING_TOKEN + GITLAB_TOKEN
```

All bot env vars map to `config.*` values; secrets come from an existing Secret (recommended) or `secrets.webhookSecret`/`secrets.signingToken`/`secrets.gitlabToken`. Set `config.authMethods` to enable signing-token auth (e.g. `secret,signature` during migration, then `signature`). The NetworkPolicy defaults allow egress to any host on 443 (gitlab.com has no stable CIDR) and to an in-cluster Prometheus in the `monitoring` namespace — tighten `networkPolicy.*` to your environment.

## Status

Bootstrap stage. Implemented and tested: the webhook intake path (validation, filtering, queueing), the report orchestration (`internal/reporter`, shared by `serve` and `run`), the GitLab API client (jobs listing, MR lookup, idempotent note upsert), and the Prometheus pod-correlation and usage queries — all exercised end-to-end in `internal/e2e` against mock GitLab and Prometheus servers. Still to come: the markdown report template (currently a placeholder), the pod-name-pattern correlation fallback, and validation of the PromQL queries against a real Prometheus snapshot. See `CLAUDE.md` for the full implementation notes.
