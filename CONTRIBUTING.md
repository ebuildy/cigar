# Contributing to CIgar

Thanks for helping out! This document covers the local tooling (mise), the development workflow, and how releases are cut.

## Prerequisites

The only thing you need to install yourself is [mise](https://mise.jdx.dev) — it manages everything else (Go, golangci-lint, goreleaser) at the exact versions pinned in [mise.toml](mise.toml):

```sh
curl https://mise.run | sh     # or: brew install mise
```

Then, from the repository root:

```sh
mise trust        # allow this repo's mise.toml
mise install      # install the pinned toolchain
```

That's it — no system-wide Go installation required, and every contributor (and CI) builds with the same tool versions.

## Everyday tasks

Tasks are defined in [mise.toml](mise.toml) and run with `mise r <task>` (short for `mise run`):

```sh
mise r build             # compile ./cmd/bot into bin/bot
mise r test              # go test -race ./... (unit + e2e)
mise r test:e2e          # only the e2e suite, verbose (mock GitLab + Prometheus)
mise r lint              # golangci-lint
mise r run               # run the bot locally (needs env vars, see README)
mise r docker            # build the container image
mise r release:snapshot  # local goreleaser dry run, artifacts in dist/
```

`mise ls` shows the installed tools, `mise tasks` lists all tasks with their descriptions.

Tool versions are deliberately pinned in `mise.toml`. If you bump one, do it in its own commit and make sure `mise r lint test build` still passes.

## Local dev cluster (optional)

`.dev/` holds a [Helmfile](https://helmfile.readthedocs.io) stack for exercising the bot against a real self-managed GitLab instance in a local [kind](https://kind.sigs.k8s.io) cluster, instead of only the e2e suite's mocks: cert-manager, [agentgateway](https://agentgateway.dev) (a Gateway API implementation) fronting GitLab CE (Omnibus) at `http://gitlab.kind.local`, and `gitlab-runner`. `kind`, `kubectl`, and `helmfile` are pinned in [mise.toml](mise.toml); you still need Docker or Podman installed yourself for kind's node containers.

If you don't already have a kind cluster, create one with host ports published for `agentgateway`'s Gateway (see below for why):

```sh
cat <<'EOF' | kind create cluster --name kind-cluster --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 80
        hostPort: 9090
      - containerPort: 443
        hostPort: 9443
EOF
```

Bring up the stack (idempotent — safe to rerun):

```sh
mise r dev:up
```

GitLab's first boot (chef reconfigure + DB migrations) takes several minutes; `kubectl get pods -n gitlab -w` shows progress. Once ready:

```sh
echo "127.0.0.1 gitlab.kind.local prometheus.kind.local" | sudo tee -a /etc/hosts   # one-time
mise r dev:gitlab:root-password                                # sign in as "root"
```

`http://gitlab.kind.local:9090` should now load — no `kubectl port-forward` needed. That works because the kind node's own port 80 is bound directly by the agentgateway proxy pod (`hostPort`, configured declaratively via the `AgentgatewayParameters` resource in [.dev/charts/agentgateway-config](.dev/charts/agentgateway-config)), and the cluster's `extraPortMappings` above publish that node port to your machine.

To register a Kubernetes-executor CI runner (via the API — no manual UI step needed):

```sh
mise r dev:gitlab:register-runner
```

Idempotent: if a `dev-k8s-runner` instance runner already exists, it's left alone (runner auth tokens, like PATs, are only ever shown once — delete it in **Admin Area → CI/CD → Runners** first to re-register from scratch). To seed an example project with a working pipeline to run against it:

```sh
mise r dev:gitlab:seed-example-project
```

Finally, to see the bot itself running against this instance — builds the image, loads it into kind, deploys a minimal Prometheus (cadvisor + kube-state-metrics) alongside it, and registers its webhook on every project (GitLab CE has no instance/group-wide pipeline webhook, so this is per-project; rerun after creating new projects to cover them too):

```sh
mise r dev:cigar:deploy
```

```sh
kubectl --context kind-kind-cluster -n cigar logs -f deploy/cigar
```

Tear down with `mise r dev:down`. See the comments in [.dev/helmfile.yaml.gotmpl](.dev/helmfile.yaml.gotmpl) for the full release graph and troubleshooting notes (e.g. why `gitlab`'s Deployment uses `strategy: Recreate` and a 900s `progressDeadlineSeconds`, and why the runner's `helper_image` is pinned to an `arm64` tag).

## Before opening a PR

The definition of done (see [CLAUDE.md](CLAUDE.md) for the full list):

1. `mise r lint` and `mise r test` are clean — the race detector is always on.
2. New PromQL queries are verified against a real Prometheus snapshot in `testdata/`, never only "by eye".
3. Webhook handler changes include tests proving invalid/missing token → `401` and oversized body → `413`.
4. Changes to the MR comment format update the golden files and the README screenshot.
5. Behavior spanning several components is covered in the e2e suite (`internal/e2e`), which runs the real webhook app, worker and API clients against mock GitLab/Prometheus servers.

CI (GitHub Actions, [.github/workflows/ci.yml](.github/workflows/ci.yml)) runs lint → test → build on every push and PR, using the same mise tasks and pinned toolchain as your machine — if it passes locally, it passes in CI.

## Releasing

Releases are fully automated with [GoReleaser](https://goreleaser.com) ([.goreleaser.yaml](.goreleaser.yaml)) and triggered by pushing a semver tag:

```sh
git tag v0.2.0
git push origin v0.2.0
```

The release workflow ([.github/workflows/release.yml](.github/workflows/release.yml)) then:

1. builds static `bot` binaries for linux/darwin × amd64/arm64, with the version stamped in (`bot --version`);
2. packages tar.gz archives and a `checksums.txt`;
3. generates a changelog from the commit messages (`docs`/`test`/`chore`/`ci` prefixes are excluded — one more reason to write [conventional-style](https://www.conventionalcommits.org) commit messages);
4. publishes it all as a GitHub release for the tag.

No credentials to set up: the workflow uses the repository's built-in `GITHUB_TOKEN`.

### Testing a release locally

Before tagging, you can validate the whole release build without publishing anything:

```sh
mise r release:snapshot   # writes archives + checksums to dist/ (gitignored)
```

Use it whenever you touch `.goreleaser.yaml`, build flags, or anything the release pipeline depends on.

## Code conventions

The short version (full details in [CLAUDE.md](CLAUDE.md)):

- Go standard library first; new dependencies need a good reason.
- Errors are wrapped with `fmt.Errorf("...: %w", err)`; no `panic` outside `main`.
- Every outbound call takes a `context.Context` with a timeout.
- Packages talk to each other through interfaces (`metrics.Source`, `gitlab.Client`, `correlate.Resolver`) so tests can stub every boundary.
- Tests are table-driven; the webhook handler is tested through Fiber's `app.Test`.
