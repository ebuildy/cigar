#!/usr/bin/env bash
# Computes the next version from Conventional Commits since the last tag (or
# from the beginning of history if there is none) per cliff.toml's policy —
# feat -> minor, fix/perf -> patch, a breaking change -> major, everything
# else (docs/chore/refactor/test/style/ci) doesn't move the version.
#
# Writes that version's section into CHANGELOG.md, commits it, and creates an
# annotated tag locally. Never pushes — review with `git log`/`git tag -v`
# and push yourself with `git push --follow-tags` when ready; pushing the tag
# is what triggers .github/workflows/release.yml. Invoked by `mise run
# release:tag`.
set -euo pipefail

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "working tree is dirty; commit or stash before tagging a release" >&2
  exit 1
fi

version_output=$(git-cliff --bumped-version 2>&1)
if echo "$version_output" | grep -q "nothing to bump"; then
  echo "No feat/fix/perf/breaking commits since the last tag; nothing to release."
  exit 0
fi
version=$(echo "$version_output" | tail -1)

if git rev-parse --verify --quiet "$version" >/dev/null; then
  echo "tag $version already exists" >&2
  exit 1
fi

echo "Next version: $version"
git-cliff --tag "$version" -o CHANGELOG.md

git add CHANGELOG.md
# --signoff: this repo's DCO check requires it on every commit, including
# this generated one (whether run locally or by .github/workflows/tag.yml).
git commit -q -s -m "chore(release): $version"
git tag -a "$version" -m "$version"

echo "Committed CHANGELOG.md and created tag $version locally."
echo "Review it, then publish with: git push --follow-tags"
