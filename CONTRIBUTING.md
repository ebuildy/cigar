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
