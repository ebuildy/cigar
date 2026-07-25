# Design: YAML config file with env + flag overrides

**Date:** 2026-07-25
**Status:** Approved (pending spec review)

## Problem

Configuration is loaded purely from ~17 environment variables in
`internal/config/config.go`. The env surface has grown unwieldy. We want a
`config.yaml` file as the primary source of configuration, with every setting
still overridable by an environment variable and by a root command flag.

## Goals

- A grouped `config.yaml` file is the primary config source.
- Every non-secret setting is overridable by an env var and a CLI flag.
- Names are **mechanically derivable** from a single yaml path, in all three
  forms.
- Secrets never live in the file — env-only, backed by a Kubernetes Secret.
- The Helm chart renders the file into a **ConfigMap** and mounts it.
- `bot run` keeps working with no file (env + defaults), per CLAUDE.md.

## Non-goals

- No change to what any setting *means* or to the existing validation rules.
- No new settings.
- No backward-compatible env aliases — this is a clean rename (pre-1.0).

## Naming convention (single rule)

Every setting has one canonical yaml path. The other two forms are pure
transforms of it:

| Form | Rule | Example |
|---|---|---|
| yaml key | `group.key`, snake_case | `prometheus.scrape_interval` |
| env var | uppercase, dots → `_` | `PROMETHEUS_SCRAPE_INTERVAL` |
| CLI flag | kebab, dots & `_` → `-` | `--prometheus-scrape-interval` |

Snake_case yaml keys are required so the env transform is a pure
uppercase-and-replace (no camelCase word-splitting guesswork).

## Precedence

viper's built-in ordering, highest first:

```
--flag  >  ENV_VAR  >  config.yaml  >  built-in default
```

## Settings mapping

Non-secret settings (rendered into the file / ConfigMap, bound to env + flag):

| yaml path | env | flag | default |
|---|---|---|---|
| `gitlab.url` | `GITLAB_URL` | `--gitlab-url` | `https://gitlab.com` |
| `prometheus.url` | `PROMETHEUS_URL` | `--prometheus-url` | *(required)* |
| `prometheus.scrape_interval` | `PROMETHEUS_SCRAPE_INTERVAL` | `--prometheus-scrape-interval` | `30s` |
| `pod_resolver` | `POD_RESOLVER` | `--pod-resolver` | `trace` |
| `webhook.auth_methods` | `WEBHOOK_AUTH_METHODS` | `--webhook-auth-methods` | `[secret]` |
| `report.throttle_warn_ratio` | `REPORT_THROTTLE_WARN_RATIO` | `--report-throttle-warn-ratio` | `0.25` |
| `report.long_job_duration` | `REPORT_LONG_JOB_DURATION` | `--report-long-job-duration` | `10m` |
| `report.memory_pressure_ratio` | `REPORT_MEMORY_PRESSURE_RATIO` | `--report-memory-pressure-ratio` | `0.9` |
| `commands.enabled` | `COMMANDS_ENABLED` | `--commands-enabled` | `false` |
| `commands.chart_format` | `COMMANDS_CHART_FORMAT` | `--commands-chart-format` | `png` |
| `server.listen_addr` | `SERVER_LISTEN_ADDR` | `--server-listen-addr` | `:8080` |
| `server.ops_addr` | `SERVER_OPS_ADDR` | `--server-ops-addr` | `:8081` |
| `log.level` | `LOG_LEVEL` | `--log-level` | `info` |

**Renamed env vars (breaking):** `SCRAPE_INTERVAL` → `PROMETHEUS_SCRAPE_INTERVAL`,
`AUTH_METHODS` → `WEBHOOK_AUTH_METHODS`, `THROTTLE_WARN_RATIO` →
`REPORT_THROTTLE_WARN_RATIO`, `LONG_JOB_DURATION` → `REPORT_LONG_JOB_DURATION`,
`MEMORY_PRESSURE_RATIO` → `REPORT_MEMORY_PRESSURE_RATIO`, `CHART_FORMAT` →
`COMMANDS_CHART_FORMAT`, `LISTEN_ADDR` → `SERVER_LISTEN_ADDR`, `OPS_ADDR` →
`SERVER_OPS_ADDR`. Unchanged: `GITLAB_URL`, `PROMETHEUS_URL`, `POD_RESOLVER`,
`COMMANDS_ENABLED`, `LOG_LEVEL`.

Secret settings — **env-only**, no file key, no flag. Their env names already fit
the convention as conceptual paths:

| conceptual path | env | required when |
|---|---|---|
| `webhook.secret` | `WEBHOOK_SECRET` | `serve` and auth_methods includes `secret` |
| `webhook.signing_token` | `WEBHOOK_SIGNING_TOKEN` | `serve` and auth_methods includes `signature` |
| `gitlab.token` | `GITLAB_TOKEN` | always |
| `commands.signing_key` | `COMMANDS_SIGNING_KEY` | `serve` and `commands.enabled` |

## config.yaml schema

```yaml
gitlab:
  url: https://gitlab.com
prometheus:
  url: http://prometheus-operated.monitoring.svc:9090
  scrape_interval: 30s
pod_resolver: trace
webhook:
  auth_methods: [secret]         # yaml list; env WEBHOOK_AUTH_METHODS stays comma-separated
report:
  throttle_warn_ratio: 0.25
  long_job_duration: 10m
  memory_pressure_ratio: 0.9
commands:
  enabled: false
  chart_format: png
server:
  listen_addr: ":8080"
  ops_addr: ":8081"
log:
  level: info
```

A commented `config.yaml` lives at the repo root. It holds only non-secret
defaults, is git-tracked, and doubles as (a) the reference/sample and (b) the
default config picked up during local `bot run`/`bot serve` (since `./config.yaml`
is first in the search path).

## Architecture

### internal/config

Introduce viper as the loader. The exported surface:

- `config.BindFlags(cmd *cobra.Command)` — registers one **persistent flag per
  non-secret setting** on the root command (so `serve` and `run` inherit them).
  `--log-level` is registered here too (moved from `main.go`, same name/behavior).
- `config.Load(cmd *cobra.Command) (*Config, error)` — builds/reuses the viper
  instance: `SetDefault` for each key; read the config file; `BindEnv(key,
  ENV_NAME)` per key to pin exact env names; `BindPFlags`; then extract each
  field via `v.GetString/GetFloat64/GetDuration/GetBool` and run the **existing
  validation** (ratio ranges, positive durations, `parseAuthMethods`,
  `pod_resolver`/`chart_format` enums, required-field checks). Secrets are read
  via `os.Getenv` directly (env-only).

The `Config` struct keeps its current fields; only the source changes. viper is
added to `go.mod` and to the approved-deps list in CLAUDE.md (spf13 companion to
the cobra dependency already in use).

`auth_methods` normalization: the file provides a yaml list, env provides a
comma-separated string (unchanged format). A small helper accepts both — reads
the viper value, and if it is a string, runs the existing comma-split
`parseAuthMethods`; if it is already a slice, validates each element against
`validAuthMethods`.

### File discovery

- `--config` root persistent flag, also settable via `CIGAR_CONFIG` env.
- Default search path when the flag is unset: `./config.yaml`, then
  `/etc/cigar/config.yaml`.
- **Optional:** no file found → env + defaults only (today's behavior). A file
  that is present but malformed is a hard error.

### Log-level wiring

`--log-level` currently builds the logger in the root `PersistentPreRunE` before
subcommands run. To let the file supply `log.level`:

1. `PersistentPreRunE` constructs the viper instance (read file, bind env, bind
   the persistent flags), resolves `log.level` through it, builds the logger, and
   stashes the viper on the command context.
2. `serve` / `run` call `config.Load(cmd)`, which reuses that viper, unmarshals
   the rest, and runs required-field validation lazily.

This keeps validation out of `--help` / `--version` (which skip `RunE`), while
making the log level file-aware. Reading an optional/missing config file never
errors for unrelated commands.

## Helm chart changes

- **New `templates/configmap.yaml`** — renders the grouped `config.yaml` from the
  restructured `values.config.*` (non-secret keys only).
- **`templates/deployment.yaml`**:
  - Mount the ConfigMap read-only at `/etc/cigar/config.yaml` (a default search
    path, so no `--config` arg needed). Compatible with `readOnlyRootFilesystem`.
  - **Remove** all non-secret `env:` entries (now sourced from the file).
  - **Keep** the four secret `env:` entries (`WEBHOOK_SECRET`,
    `WEBHOOK_SIGNING_TOKEN`, `GITLAB_TOKEN`, `COMMANDS_SIGNING_KEY`) from the
    Secret, plus `extraEnv`.
  - Add a `checksum/config` pod annotation (sha256 of the rendered ConfigMap) so
    a config change rolls the pods.
- **`values.yaml`** — restructure `config.*` into the grouped shape mirroring
  `config.yaml` (e.g. `config.prometheus.scrapeInterval`,
  `config.report.throttleWarnRatio`). Secret-related `commands.enabled` moves
  under `config.commands.enabled`. `secrets.*` unchanged.

## Testing

- `internal/config/config_test.go`: table-driven tests proving
  - precedence (file < env < flag) for a representative setting,
  - file parse of the grouped schema,
  - missing-file fallback to env + defaults,
  - malformed-file error,
  - validation still fires (bad ratio, bad duration, unknown pod_resolver,
    unknown auth method, missing required field),
  - `auth_methods` accepts both a yaml list and a comma env string.
- `internal/e2e` is env-driven; update it to the renamed env vars and confirm it
  still passes unchanged in behavior.
- **Test isolation from the root `config.yaml`:** tests must not accidentally pick
  up the repo-root `./config.yaml`. `internal/config` and `e2e` tests run from
  their own package dir (not repo root), so `./config.yaml` is absent there; tests
  that need a specific file pass an explicit path. Confirm no test's effective cwd
  is the repo root, and that `config.Load` with no file + full env still behaves
  as today.
- `helm lint` + `helm template` to verify the ConfigMap, mount, and checksum
  annotation render; confirm no non-secret env entries remain.

## Documentation

- `README.md`: replace the env-var table with the mapping table above and a
  `config.yaml` example; note the breaking renames.
- `docs/usage.md`: add a "Configuration" section (file, precedence, discovery).
- Root `config.yaml` is committed as the reference/default (created).
- CLAUDE.md: update the Config section (env → file + precedence) and add viper to
  approved deps.

## Definition of done

- `mise r lint test` clean, race on.
- `helm lint` + `helm template` clean; deployment has no non-secret env vars and
  mounts the ConfigMap.
- README env table and docs updated; sample file committed.
- Config precedence covered by tests.
