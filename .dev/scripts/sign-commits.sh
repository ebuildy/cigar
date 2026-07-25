#!/usr/bin/env bash
# Verifies that every commit on the current branch (relative to the base
# branch, default "main") carries a signature, and if any do not, rebases the
# branch in place to sign them all. Override the base branch with BASE_BRANCH.
#
# Rewrites history when it signs, so it refuses to run over a dirty working
# tree and reminds you to force-push afterwards. Invoked by `mise run
# sign-commits`.
set -euo pipefail

base_branch="${BASE_BRANCH:-main}"

if ! git rev-parse --verify --quiet "$base_branch" >/dev/null; then
  echo "base branch '$base_branch' not found (set BASE_BRANCH)" >&2
  exit 1
fi

base=$(git merge-base "$base_branch" HEAD)
if [ "$base" = "$(git rev-parse HEAD)" ]; then
  echo "No commits on this branch beyond $base_branch; nothing to check."
  exit 0
fi

count=$(git rev-list --count "$base"..HEAD)

# %G? is 'N' for a commit with no signature; anything else means it carries one.
unsigned=$(git log --format='%H %G?' "$base"..HEAD | awk '$2 == "N" { print $1 }')
if [ -z "$unsigned" ]; then
  echo "All $count branch commit(s) are signed."
  exit 0
fi

echo "Unsigned commit(s) on this branch:"
git log --format='  %h %s' "$base"..HEAD | while read -r line; do echo "$line"; done

# Rebasing rewrites history, so refuse to run over uncommitted changes.
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "working tree is dirty; commit or stash before signing" >&2
  exit 1
fi

echo "Rebasing $count commit(s) onto $base to sign them..."
git rebase --force-rebase --gpg-sign "$base"

# Fail loudly if anything is still unsigned (e.g. signing not configured).
still=$(git log --format='%H %G?' "$base"..HEAD | awk '$2 == "N" { print $1 }')
if [ -n "$still" ]; then
  echo "some commits are still unsigned after rebase; check your git signing config" >&2
  exit 1
fi
echo "Done. All $count branch commit(s) are signed."
echo "This branch's history was rewritten — force-push with: git push --force-with-lease"
