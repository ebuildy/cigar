# Deploying with Helm

The chart lives in [`deploy/chart/cigar`](../deploy/chart/cigar). It is not
published to a repository yet — deploy it from a checkout of this repo.

For what the bot does once it is running (webhook registration, report format,
interactive commands), see [`usage.md`](usage.md).

## Prerequisites

- A Kubernetes cluster and Helm 3+.
- A Prometheus that scrapes **cadvisor** (`container_*` series) and
  **kube-state-metrics** (`kube_pod_container_resource_*`), reachable from the
  bot's namespace. Without kube-state-metrics the report still renders, but the
  request/limit columns and the over-provisioning advice stay empty.
- A GitLab API token with the `api` scope — a project or group access token,
  least privilege.
- HTTPS termination in front of the Service (Ingress or Gateway API). The pod
  itself listens on plain HTTP.

## Quick start

```sh
kubectl create namespace cigar

kubectl -n cigar create secret generic cigar-secrets \
  --from-literal=GITLAB_TOKEN=glpat-… \
  --from-literal=WEBHOOK_SIGNING_TOKEN="whsec_$(openssl rand -base64 32)"
  # ^ whsec_ + standard base64 of a 32-byte key. Keep it — the same value goes
  #   on every GitLab project hook as its signing_token.

helm upgrade --install cigar deploy/chart/cigar -n cigar \
  --set config.gitlab.url=https://gitlab.example.com \
  --set config.prometheus.url=http://prometheus-operated.monitoring.svc:9090 \
  --set config.webhook.authMethod=signing_token \
  --set secrets.GITLAB_TOKEN.existingSecret=cigar-secrets \
  --set secrets.WEBHOOK_SIGNING_TOKEN.existingSecret=cigar-secrets \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=cigar.example.com
```

Then register the webhook on your GitLab projects, pointing at
`https://cigar.example.com/webhook` with the same signing token — see
[`usage.md`](usage.md#2-gitlab-configuration).

## What the chart ships

| Resource | Notes |
| --- | --- |
| `Deployment` | `runAsNonRoot` uid 65532, read-only rootfs, all capabilities dropped, seccomp `RuntimeDefault` |
| `ConfigMap` | `config.*` values rendered to `/etc/cigar/config.yaml`; its checksum annotates the pod template, so a config change rolls the pods |
| `Secret` | only for secrets given as a literal `secrets.<NAME>.value` |
| `ExternalSecret` / generators | one per `externalSecrets` / `externalSecretsGenerator` entry, specs passed through verbatim |
| `Service` | ClusterIP, `8080` (`http`, webhook) + `8081` (`ops`, health & metrics) |
| `Ingress` / `HTTPRoute` | both disabled by default; enable one |
| `NetworkPolicy` | enabled by default — see below |
| `PodDisruptionBudget` | `minAvailable: 1` |
| `PodMonitor` | disabled by default (needs the Prometheus Operator CRDs) |
| `ServiceAccount` | created, with `automountServiceAccountToken: false` — the bot never calls the Kubernetes API |

`replicaCount` defaults to **1**. The PDB's `minAvailable: 1` then makes a node
drain block until you scale up or delete it, so set `replicaCount: 2` for any
cluster where nodes get drained. Two replicas are safe: the MR note is upserted
by marker, so a duplicate delivery cannot produce a duplicate comment.

## Configuration

Everything under `config.*` is rendered into the ConfigMap and read from
`/etc/cigar/config.yaml`. Each key also has an env var and a flag form
(`gitlab.url` → `GITLAB_URL` → `--gitlab-url`), with precedence
**flag > env > file > default** — use `extraEnv` for per-environment overrides
without re-templating.

```yaml
config:
  gitlab:
    url: https://gitlab.example.com
  prometheus:
    url: http://prometheus-operated.monitoring.svc:9090
    # Pad query windows by one scrape interval; must match your Prometheus.
    scrapeInterval: "30s"
  # "trace" (parse the job log's "Running on <pod>" line — no pod labels
  # needed) or "prometheus" (join kube_pod_labels on a job_id pod label).
  podResolver: trace
  webhook:
    authMethod: signing_token
  report:
    throttleWarnRatio: "0.25"
    longJobDuration: "10m"
    memoryPressureRatio: "0.9"
    compare:
      enabled: true
      durationDeltaRatio: "0.05"
      historyPipelines: "6"
      cacheTtl: "1h"
  commands:
    enabled: false
    chartFormat: png
  log:
    level: info
```

If `config.gitlab.url`'s host does not resolve the same way inside the cluster,
add `hostAliases` rather than bending DNS.

## Secrets

The bot reads up to four secret values, and the chart injects only the ones the
current configuration needs:

| Env var | Needed when |
| --- | --- |
| `GITLAB_TOKEN` | always |
| `WEBHOOK_SECRET` | `config.webhook.authMethod: secret` (the default) |
| `WEBHOOK_SIGNING_TOKEN` | `config.webhook.authMethod: signing_token` |
| `COMMANDS_SIGNING_KEY` | `config.commands.enabled: true` |

So choosing `signing_token` does *not* require you to also carry a
`WEBHOOK_SECRET`. A secret the configuration doesn't need is ignored even if you
configure it — it stays out of the pod's environment and out of any Secret.

Each one is configured under `secrets.<ENV_VAR>` and sourced **independently**,
so the GitLab token can live in a Secret your platform team owns while the HMAC
keys come from somewhere else entirely. Set exactly one source per secret:

| Source | Where the value ends up |
| --- | --- |
| `value` | a Secret this chart owns, named after the release |
| `existingSecret` | a Secret you already manage, read as-is |
| `externalSecret` | name of an `externalSecrets` entry; reads the Secret that `ExternalSecret` manages |

A required secret with no source, or with more than one, fails the render.

```yaml
# The GitLab PAT from a Secret the platform team owns, under its own key name;
# the webhook secret as a literal for a scratch cluster.
secrets:
  GITLAB_TOKEN:
    existingSecret: gitlab-creds
    key: token          # defaults to GITLAB_TOKEN
  WEBHOOK_SECRET:
    value: "s3cr3t"
```

That renders one `secretKeyRef` per secret, each pointing wherever it came from:

```yaml
env:
  - name: GITLAB_TOKEN
    valueFrom:
      secretKeyRef: {name: gitlab-creds, key: token}
  - name: WEBHOOK_SECRET
    valueFrom:
      secretKeyRef: {name: cigar, key: WEBHOOK_SECRET}
```

Because there is no default source, `helm template`/`helm lint` on bare defaults
fails with `secrets.GITLAB_TOKEN needs a source` — pass a placeholder
(`--set secrets.GITLAB_TOKEN.value=x --set secrets.WEBHOOK_SECRET.value=x`) when
you just want to inspect the manifests.

### The signing token's shape

`WEBHOOK_SIGNING_TOKEN` is the one secret with a fixed format: **`whsec_`
followed by standard base64 of a 32-byte key** (44 base64 characters ending in
`=`). The bot strips the prefix and base64-decodes the rest into its HMAC-SHA256
key; GitLab rejects a hook whose `signing_token` isn't in that form with a `422`.
Generate one with:

```sh
echo "whsec_$(openssl rand -base64 32)"
# whsec_ZuaImjLinOp9o9OBSkNkHUO3vxdglJgGqhmibDg9Vek=
```

The same string must be set on the bot *and* on every project hook — rotating
one without the other 401s every delivery. A literal `value` is checked against
that shape at render time; behind an `existingSecret` or an `ExternalSecret` it
is on you — see the generator example below for producing a valid one.

### `value` — literals

```yaml
secrets:
  GITLAB_TOKEN:
    value: glpat-…
  WEBHOOK_SIGNING_TOKEN:
    value: whsec_ZuaImjLinOp9o9OBSkNkHUO3vxdglJgGqhmibDg9Vek=
```

Every literal lands in one Secret the chart owns, keyed by env var name.
Convenient for a scratch cluster, but the values end up in your Helm release and
in whatever holds your `values.yaml` — don't commit real ones.

### `existingSecret` — a Secret you manage

```yaml
secrets:
  GITLAB_TOKEN:
    existingSecret: gitlab-creds
    key: token          # optional; defaults to GITLAB_TOKEN
```

The chart creates nothing and reads that Secret directly. This is the right
choice when something outside Helm owns the value — sealed-secrets, SOPS, a
bootstrap script, your platform team, or an `ExternalSecret` you wrote yourself
against a `ClusterSecretStore`. Different secrets may name different Secrets.

### `externalSecret` — a Secret the operator manages

`externalSecrets` is a map of **raw [External Secrets Operator](https://external-secrets.io)
`ExternalSecret` specs**, rendered verbatim. The chart doesn't model ESO at all,
so everything ESO supports works — stores, generators, `dataFrom`, templates,
rewrites, `find`/`extract` — without waiting for chart support.

Each entry becomes a CR named `<release>-<key>`, and ESO defaults the Secret it
manages to that same name. A secret then points at the entry by name:

```yaml
externalSecrets:
  vault:
    refreshInterval: "1h"
    secretStoreRef:
      name: vault
      kind: ClusterSecretStore
    data:
      - secretKey: GITLAB_TOKEN
        remoteRef:
          key: cigar/gitlab
          property: token

secrets:
  GITLAB_TOKEN:
    externalSecret: vault        # reads Secret `cigar-vault`, key GITLAB_TOKEN
```

The chart checks the name resolves to a declared entry — a typo fails the render
with the list of what *is* defined — and otherwise stays out of the way. Set
`spec.target.name` yourself if you want the Secret named something else, and
`externalSecretApiVersion: external-secrets.io/v1beta1` for operators older than
ESO 0.11.

#### Generating values

`externalSecretsGenerator` creates ESO generators alongside them. The entry is
the `passwordSpec` of a `Password` `ClusterGenerator` — the common case, since
one generator mints an independent value per `secretKeys` entry:

```yaml
externalSecretsGenerator:
  cigar-keys:
    # 32 raw characters — see the whsec_ template below.
    length: 32
    digits: 8
    symbols: 0            # these travel through env vars; keep them quoting-safe
    secretKeys:
      - WEBHOOK_SIGNING_TOKEN
      - COMMANDS_SIGNING_KEY
```

Names are used verbatim (not release-prefixed) so they match the
`sourceRef.generatorRef.name` in your specs. `ClusterGenerator` is
cluster-scoped, so keep them unique across releases. `allowRepeat` and `noUpper`
are required by the CRD and defaulted for you. For any other generator kind,
give a raw `spec` instead:

```yaml
externalSecretsGenerator:
  uid:
    kind: ClusterGenerator
    spec:
      kind: UUID
      generator:
        uuidSpec: {}
```

One `ExternalSecret` then pulls every key the generator emits with `dataFrom`:

```yaml
externalSecrets:
  keys:
    # A generator mints NEW values on every refresh. "0s" fetches once, and ESO
    # persists the result in a GeneratorState — essential for anything GitLab
    # also holds a copy of.
    refreshInterval: "0s"
    dataFrom:
      - sourceRef:
          generatorRef:
            apiVersion: generators.external-secrets.io/v1alpha1
            kind: ClusterGenerator
            name: cigar-keys
    target:
      template:
        engineVersion: v2
        # Keeps untemplated keys: ESO's default (Replace) would drop
        # COMMANDS_SIGNING_KEY, which the template below doesn't mention.
        mergePolicy: Merge
        data:
          # A generator emits a bare random string, but GitLab signing tokens are
          # whsec_<standard base64 of a 32-byte key>. 32 raw chars -> b64enc ->
          # 44 base64 chars -> a 32-byte key. Don't also set the generator's
          # `encoding`, or it double-encodes.
          WEBHOOK_SIGNING_TOKEN: "whsec_{{ .WEBHOOK_SIGNING_TOKEN | b64enc }}"

secrets:
  WEBHOOK_SIGNING_TOKEN:
    externalSecret: keys
  COMMANDS_SIGNING_KEY:
    externalSecret: keys
```

Two things the chart can no longer check for you, both worth knowing:

- **`GITLAB_TOKEN` can't be generated.** It is a project or group access token
  with `api` scope that you issue in GitLab; a random value authenticates
  nothing. Back it with a store, or an `existingSecret`.
- **A generated signing token still has to reach GitLab.** It is well-formed,
  but until you copy it onto each project hook's `signing_token`, every delivery
  401s. Read it back with:

  ```sh
  kubectl -n cigar get secret cigar-keys \
    -o jsonpath='{.data.WEBHOOK_SIGNING_TOKEN}' | base64 -d
  ```

`.dev/` is a working reference for all of this — see the `cigar` release in
[`.dev/helmfile.yaml.gotmpl`](../.dev/helmfile.yaml.gotmpl), where
`deploy-cigar.sh` reads the generated token back out and registers it on every
project hook.

### Render-time checks

The chart refuses to produce something that would only fail once the pod is
running:

- A required secret with no source, or with more than one.
- A `secrets.*` key that isn't one of the four names — catches typos.
- An `externalSecret` naming no entry under `externalSecrets` — the error lists
  what is defined, so a typo is obvious.
- A literal `WEBHOOK_SIGNING_TOKEN` that isn't `whsec_<base64 of 32 bytes>`.

## Exposure

Enable exactly one of `ingress` or `httpRoute`. Only `/webhook` needs to be
reachable from GitLab — the ops port carries health and metrics and should stay
internal.

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: cigar.example.com
      paths:
        - path: /webhook
          pathType: Prefix
  tls:
    - secretName: cigar-tls
      hosts:
        - cigar.example.com
```

For Gateway API, `httpRoute.enabled: true` with `parentRefs` pointing at your
Gateway (see the dev stack's release for a worked example).

## NetworkPolicy

Enabled by default, and deliberately loose where it has to be: egress to GitLab
defaults to `0.0.0.0/0` on 443, because gitlab.com publishes no stable CIDR.
Tighten it for a self-hosted instance, and point `prometheus` at wherever your
Prometheus actually runs — the defaults assume the `monitoring` namespace.

```yaml
networkPolicy:
  enabled: true
  ingressFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: ingress-nginx
  gitlab:
    cidr: 10.20.0.0/16
    port: 443
  prometheus:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: monitoring
    podSelector:
      matchLabels:
        app.kubernetes.io/name: prometheus
    port: 9090
```

Egress is otherwise limited to DNS. If the bot has to reach anything else
(a proxy, an object store for command chart uploads), add it to `extraEgress`.

## Metrics

The bot exposes its own `cigar_*` metrics on the ops port. With the Prometheus
Operator installed:

```yaml
podMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack   # whatever your Prometheus selects on
```

See [`monitoring.md`](monitoring.md) for the metric reference and alert
examples, and [`deploy/grafana/cigar-dashboard.json`](../deploy/grafana/cigar-dashboard.json)
for a ready-made dashboard.

## Validating chart changes

```sh
mise r helm:test   # helm lint + the helm-unittest suites in deploy/chart/cigar/tests
```

The suites cover the ConfigMap render and every secret path (chart-managed,
existing Secret, ExternalSecret in both store and generator mode, plus each
render-time check). `mise r helm:template` prints the manifests for eyeballing.

## Local dev cluster

`.dev/` defines a complete kind stack — GitLab, a Kubernetes-executor runner,
kube-prometheus-stack, the External Secrets Operator, and the bot itself:

```sh
mise r dev:up             # bring up the stack
mise r dev:cigar:deploy   # build, load and deploy the bot, register webhooks
```

See [`usage.md`](usage.md#local-dev-cluster) for the full walkthrough.
