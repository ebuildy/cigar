# Usage

`cigar` receives GitLab **Pipeline event** webhooks, queries Prometheus for the
resource usage of the Kubernetes runner pods that ran the pipeline's jobs, and
posts (or updates) a single merge-request comment summarizing CPU, memory,
throttling and network usage.

This guide covers **deployment**, **GitLab configuration**, and **testing**.

---

## Configuration

cigar is configured by a `config.yaml` file, with env vars and CLI flags
overriding it. The three forms of every non-secret setting derive from one yaml
path: `group.key` → `GROUP_KEY` → `--group-key`. Precedence, highest first:

```
--flag  >  ENV_VAR  >  config.yaml  >  built-in default
```

**File discovery:** `--config <path>` or `$CIGAR_CONFIG`, else `./config.yaml`,
else `/etc/cigar/config.yaml`. No file found → env + defaults only.

**Secrets are environment-only** and never read from the file: `WEBHOOK_SECRET`,
`WEBHOOK_SIGNING_TOKEN`, `GITLAB_TOKEN`, `COMMANDS_SIGNING_KEY`.

See the annotated [`config.yaml`](../config.yaml) at the repo root for every key.
In Kubernetes the Helm chart renders these into a ConfigMap mounted at
`/etc/cigar/config.yaml`.

---

## 1. Deployment

### Configuration (environment variables)

The bot is 12-factor: it reads everything from the environment and fails fast at
startup on missing/invalid required values.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `GITLAB_URL` | no | `https://gitlab.com` | GitLab base URL |
| `GITLAB_TOKEN` | **yes** | — | GitLab API token, `api` scope (create/update MR notes) |
| `PROMETHEUS_URL` | **yes** | — | Prometheus base URL (cadvisor + kube-state-metrics) |
| `POD_RESOLVER` | no | `trace` | Pod-correlation strategy: `trace` (parse the job's GitLab trace) or `prometheus` (join `kube_pod_labels{label_job_id}`) |
| `WEBHOOK_AUTH_METHOD` | no | `secret` | Webhook auth method: `secret` or `signing_token` |
| `WEBHOOK_SECRET` | with `secret` | — | Legacy shared secret, compared against the `X-Gitlab-Token` header |
| `WEBHOOK_SIGNING_TOKEN` | with `signing_token` | — | GitLab signing token (`whsec_…`) used to verify the `webhook-signature` HMAC |
| `REPORT_THROTTLE_WARN_RATIO` | no | `0.25` | Throttled-periods ratio above which a warning is shown |
| `PROMETHEUS_SCRAPE_INTERVAL` | no | `30s` | Prometheus scrape interval; query windows are padded by one interval |
| `REPORT_LONG_JOB_DURATION` | no | `10m` | Job duration above which `advise` suggests splitting the job |
| `REPORT_MEMORY_PRESSURE_RATIO` | no | `0.9` | Peak-memory-to-limit ratio above which `advise` warns about OOMKill risk |
| `SERVER_LISTEN_ADDR` | no | `:8080` | Webhook HTTP listener |
| `SERVER_OPS_ADDR` | no | `:8081` | Ops listener: `/healthz`, `/readyz`, `/metrics` |
| `LOG_LEVEL` | no | `info` | `debug` \| `info` \| `warn` \| `error` — structured JSON logs (zap) written to stdout; also settable per-invocation with the `--log-level` root flag, which takes precedence |
| `COMMANDS_ENABLED` | no | `false` | Turn on [interactive report commands](#4-interactive-report-commands) (reply-driven `help` / `details`) |
| `COMMANDS_SIGNING_KEY` | when `COMMANDS_ENABLED=true` | — | HMAC key signing the report marker; must be a stable random secret shared by every replica (`serve` only — `bot run` never needs it) |
| `COMMANDS_CHART_FORMAT` | no | `png` | Format for `details` charts: `png` (renders inline reliably in GitLab), `svg` (vector; GitLab's inline SVG rendering is unreliable), or `markdown` (an ASCII line chart embedded directly in the reply — no upload) |

**Webhook authentication.** `WEBHOOK_AUTH_METHOD` selects the single active
method; a request it does not authenticate is rejected with `401`.

- `secret` — constant-time compare of the `X-Gitlab-Token` header against
  `WEBHOOK_SECRET`. (An empty configured secret never authenticates.)
- `signing_token` — verify the `webhook-signature` header: `HMAC-SHA256` over
  `{webhook-id}.{webhook-timestamp}.{body}` keyed by `WEBHOOK_SIGNING_TOKEN`
  (the `whsec_` prefix is stripped and the remainder base64-decoded). Deliveries
  whose timestamp is more than **5 minutes** from now are rejected (replay
  protection).

`signing_token` is the recommended method; `secret` is GitLab's legacy scheme.
To migrate without dropping deliveries, add a signing token to each GitLab hook
while leaving its secret token in place — GitLab then sends both credentials —
switch the bot to `WEBHOOK_AUTH_METHOD=signing_token`, then clear the secret
tokens in GitLab.

### Deploy with Helm

The chart lives in `deploy/chart/cigar`. Provide `GITLAB_TOKEN` and the auth
credential of the configured method via a Kubernetes Secret — either chart-managed
(`secrets.webhookSecret` / `secrets.signingToken` / `secrets.gitlabToken`) or,
recommended for production, an externally-managed `secrets.existingSecret`.

```sh
# Example: signing-token auth, credentials in an existing Secret.
helm upgrade --install cigar deploy/chart/cigar \
  --set config.gitlab.url=https://gitlab.example.com \
  --set config.prometheus.url=http://prometheus-server.monitoring.svc:80 \
  --set config.webhook.authMethod=signing_token \
  --set secrets.existingSecret=cigar-secrets
```

The referenced Secret must carry the key the configured method needs:

```sh
kubectl -n cigar create secret generic cigar-secrets \
  --from-literal=GITLAB_TOKEN=glpat-… \
  --from-literal=WEBHOOK_SIGNING_TOKEN="whsec_$(openssl rand -base64 32)"
  # use --from-literal=WEBHOOK_SECRET=… instead for authMethod=secret
```

The chart injects only the env var its method needs: `WEBHOOK_SECRET` for
`authMethod: secret` (the default), `WEBHOOK_SIGNING_TOKEN` for
`authMethod: signing_token`. So choosing `signing_token` does **not** require an
existing-secret user to also carry `WEBHOOK_SECRET`. Any other value fails the
`helm template`/`install` with an explicit error.

The chart also ships a Deployment (2 replicas + PDB), Service (`8080` http,
`8081` ops), Ingress (TLS), NetworkPolicy, and hardened pod security
(`runAsNonRoot`, read-only rootfs, seccomp `RuntimeDefault`). Validate changes
with `helm lint deploy/chart/cigar` and `helm template deploy/chart/cigar`.

Build the image with `mise r docker` (multi-stage, distroless/static, nonroot);
the published image is `ghcr.io/ebuildy/cigar`.

### Local dev cluster

A full local stack (kind + GitLab + a Kubernetes-executor runner + Prometheus)
is defined in `.dev/`. One command builds the image, loads it into kind,
deploys the bot wired to the dev GitLab/Prometheus, and registers the webhook on
every project:

```sh
mise r dev:up               # bring up the kind stack (first time)
mise r dev:cigar:deploy     # build + load + deploy + register webhooks (idempotent)
kubectl -n cigar logs -f deploy/cigar
```

`deploy-cigar.sh` mints and persists a stable `WEBHOOK_SIGNING_TOKEN` in
`cigar-secrets`, deploys with `WEBHOOK_AUTH_METHOD=signing_token`, and sets a matching
`signing_token` on each project hook.

---

## 2. GitLab configuration

For each project (GitLab CE has no instance- or group-wide pipeline webhook, so
registration is per-project), configure a webhook:

- **URL** → the bot's webhook endpoint, e.g.
  `https://cigar.example.com/webhook` (in-cluster: `http://cigar.cigar.svc.cluster.local:8080/webhook`).
- **Trigger** → **Pipeline events** only.
- **Authentication** → set the credential matching your `WEBHOOK_AUTH_METHOD`:
  - **Signing token** (recommended): a `whsec_`-prefixed, base64 value equal to
    the bot's `WEBHOOK_SIGNING_TOKEN`. GitLab **rejects** a non-`whsec_` value
    (HTTP `422`).
  - **Secret token** (legacy): an arbitrary string equal to `WEBHOOK_SECRET`.

Requirements:

- **API token** — the bot needs a GitLab token (`GITLAB_TOKEN`) with `api`
  scope (project or group access token, least privilege) to read jobs and
  create/update MR notes.
- **Local/private targets** — if the bot is reachable only at a private/cluster
  address, enable *Allow requests to the local network from webhooks* (Admin →
  Settings → Network → Outbound requests), otherwise GitLab blocks delivery
  (SSRF protection).

### Configure via the REST API

```sh
API="$GITLAB_URL/api/v4"
SIGNING_TOKEN="whsec_$(openssl rand -base64 32)"   # == the bot's WEBHOOK_SIGNING_TOKEN

# Create the hook (signing-token auth, pipeline events only):
curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  -X POST "$API/projects/<PROJECT_ID>/hooks" \
  --data-urlencode "url=https://cigar.example.com/webhook" \
  --data-urlencode "signing_token=$SIGNING_TOKEN" \
  --data-urlencode "pipeline_events=true"

# One-time, self-hosted, in-cluster bot: allow webhooks to reach local addresses.
curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  -X PUT "$API/application/settings" \
  --data-urlencode "allow_local_requests_from_web_hooks_and_services=true"
```

(For legacy secret-token auth, replace `signing_token=` with `token=`.)
The signing/secret token is write-only; the hook object reports only
`signing_token_present` / `token_present`.

---

## 3. Testing

### Trigger a delivery from GitLab

GitLab can send a sample pipeline event to the hook:

```sh
API="$GITLAB_URL/api/v4"
# Find the hook id:
curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" "$API/projects/<PROJECT_ID>/hooks"
# Send a test pipeline event:
curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  -X POST "$API/projects/<PROJECT_ID>/hooks/<HOOK_ID>/test/pipeline_events"
```

Confirm the bot accepted it (should be `200`, not `401`):

```sh
# GitLab's recorded delivery result:
curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  "$API/projects/<PROJECT_ID>/hooks/<HOOK_ID>/events" \
  | python3 -c "import json,sys;[print(e['trigger'],e['response_status']) for e in json.load(sys.stdin)]"

# The bot's own logs:
kubectl -n cigar logs deploy/cigar
```

### Confirm auth is enforced (negative check)

An unsigned/unauthenticated request must be rejected with `401`:

```sh
curl -s -o /dev/null -w "%{http_code}\n" -X POST \
  -H "X-Gitlab-Event: Pipeline Hook" -H "Content-Type: application/json" \
  --data '{"object_kind":"pipeline","object_attributes":{"id":1,"status":"success"}}' \
  https://cigar.example.com/webhook
# -> 401
```

### End-to-end on a real pipeline

Run a pipeline on a project that has an open MR. When it reaches a terminal
status (`success`/`failed`), the bot resolves the runner pods, queries
Prometheus, and posts/updates a single MR note (idempotent — it never spams a
new comment per run). In the dev cluster, seed one with:

```sh
mise r dev:gitlab:seed-load-test   # creates a project + MR whose pipeline stresses CPU/mem/network
```

Health/ops endpoints are on `:8081`: `/healthz`, `/readyz`, `/metrics`.

### Local checks

```sh
mise r test        # unit + e2e (race detector); includes signing-token auth coverage
mise r test:e2e    # end-to-end only (mock GitLab + Prometheus)
mise r lint
```

### Preview the report without GitLab

`bot run` builds the same report once and prints it to stdout (no
`WEBHOOK_SECRET`/`WEBHOOK_SIGNING_TOKEN` needed):

```sh
bot run --project <project-id> <pipeline-id>
```

---

## 4. Interactive report commands

The bot can respond to **replies on its own MR resource-report comment** with a
small set of commands. This is **off by default**.

### Enabling it

Set two environment variables (both required together; `serve` fails fast if
`COMMANDS_ENABLED=true` and `COMMANDS_SIGNING_KEY` is empty):

- `COMMANDS_ENABLED=true`
- `COMMANDS_SIGNING_KEY=<a stable random secret>` — e.g.
  `openssl rand -base64 32`. It must be **identical across all replicas**
  (it signs/verifies the marker embedded in every report, so any replica must
  be able to verify a marker written by any other).

No new GitLab scope is needed: `GITLAB_TOKEN` already has `api` scope, which
covers the discussions, uploads, and note-reply calls the command path uses.

### Commands

Post these as a **reply** inside the thread of the bot's resource-report
comment (not as a new top-level comment):

| Command | Result |
|---|---|
| `help` | Reply listing the available commands |
| `details job <name>` | Reply with CPU, memory, and network charts for the named job in this report |
| `details pod <runner-...>` | Same, for one of this report's runner pods |
| `details <name>` | Auto-detects job vs. pod — names starting with `runner-` are treated as pods, everything else as a job |
| `advise` | Reply with recommendations for every job in this report |
| `advise <job>` | Same, for one job — accepts a job name or numeric ID |

Each `details` reply embeds three charts (uploaded to the MR) covering the
target's run window. Rendered as PNG by default; set `COMMANDS_CHART_FORMAT` to `svg`
for vector images, or `markdown` for a pure-text ASCII line chart embedded
right in the reply (no upload).

#### How advice is generated

Each job is checked by an independent rule; a rule that does not apply stays
silent, and a pipeline with nothing to fix gets `You are all good dude!`.

| Rule | Fires when |
|---|---|
| `cpu-throttle` | The job was throttled at or above `REPORT_THROTTLE_WARN_RATIO` |
| `java-threads` | Throttled **and** the job trace shows a Maven/Gradle/Java build |
| `long-job` | The job ran longer than `REPORT_LONG_JOB_DURATION` |
| `memory-pressure` | Peak memory reached `REPORT_MEMORY_PRESSURE_RATIO` of the memory limit |

The `java-threads` rule is the only one that reads the job's trace, and it is
fetched only for jobs that were actually throttled — one extra API call per
throttled job, none for the rest.

The same rules back the CLI:

```sh
bot advise --project 42 987654            # every job
bot advise --project 42 987654 --job test # one job
```

### How it works / security

The bot only acts on replies inside **its own** report-comment thread, and
only for jobs/pods that belong to **that report's pipeline** — it cannot be
used to pull metrics for arbitrary pods elsewhere in the cluster:

- The reply's discussion root must be a note **authored by the bot's own
  GitLab user** (not spoofable by editing a comment to add the bot's marker).
- The root note's marker is **HMAC-signed** (`COMMANDS_SIGNING_KEY`) over the
  pipeline and MR id, so an edited/tampered marker fails verification and the
  command is dropped silently.
- The allowed targets (jobs/pods) are re-derived live from GitLab for that
  signed pipeline — never read from the note's text — so editing the report
  comment cannot widen what a command can see.

Any note that isn't a recognized command, or fails one of the checks above, is
ignored with no reply (no error is posted back).
