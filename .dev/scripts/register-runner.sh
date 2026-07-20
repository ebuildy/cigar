#!/usr/bin/env bash
# Registers a Kubernetes-executor instance runner against the dev GitLab
# (see .dev/helmfile.yaml.gotmpl) and deploys gitlab-runner with it.
# Idempotent: if a runner already exists (by description) it's left alone
# — runner auth tokens, like PATs, are only shown once at creation, so a
# rerun can't recover a lost one. Delete the runner in GitLab (Admin Area
# > CI/CD > Runners) first if you need to re-register from scratch.
set -euo pipefail

KUBE_CONTEXT="${KUBE_CONTEXT:-kind-kind-cluster}"
GITLAB_DEV_URL="${GITLAB_DEV_URL:-http://gitlab.kind.local:9090}"
RUNNER_DESCRIPTION="${RUNNER_DESCRIPTION:-dev-k8s-runner}"
RUNNER_TAGS="${RUNNER_TAGS:-kubernetes,dev}"
TOKEN_NAME="dev-bootstrap"

HELMFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/helmfile.yaml.gotmpl"

echo "==> Bootstrapping a root personal access token ($TOKEN_NAME, create_runner scope)"
PAT=$(kubectl --context "$KUBE_CONTEXT" -n gitlab exec deploy/gitlab -- gitlab-rails runner "
  user = User.find_by_username!('root')
  user.personal_access_tokens.where(name: '$TOKEN_NAME').destroy_all
  pat = user.personal_access_tokens.create!(
    name: '$TOKEN_NAME',
    scopes: %w[api create_runner],
    expires_at: 365.days.from_now
  )
  puts pat.token
" | tail -1)

if [[ -z "$PAT" || "$PAT" != glpat-* ]]; then
  echo "error: failed to obtain a personal access token (got: '$PAT')" >&2
  exit 1
fi

API="$GITLAB_DEV_URL/api/v4"

echo "==> Checking for an existing '$RUNNER_DESCRIPTION' runner"
existing=$(curl -sf -H "PRIVATE-TOKEN: $PAT" \
  "$API/runners/all?type=instance_type" \
  | grep -c "\"description\":\"$RUNNER_DESCRIPTION\"" || true)

if [[ "$existing" -gt 0 ]]; then
  echo "    already registered — leaving it alone (its token can't be recovered)."
  echo "    To re-register from scratch: delete it in GitLab (Admin Area > CI/CD > Runners), then rerun this script."
  echo "    If gitlab-runner isn't deployed yet, you'll need GITLAB_RUNNER_TOKEN from when it was created:"
  echo "      helmfile -f \"$HELMFILE\" apply -l name=gitlab-runner"
  exit 0
fi

echo "==> Creating instance runner ($RUNNER_DESCRIPTION, tags: $RUNNER_TAGS)"
resp=$(curl -sf -H "PRIVATE-TOKEN: $PAT" \
  -X POST "$API/user/runners" \
  --data-urlencode "runner_type=instance_type" \
  --data-urlencode "description=$RUNNER_DESCRIPTION" \
  --data-urlencode "tag_list=$RUNNER_TAGS")

RUNNER_TOKEN=$(printf '%s' "$resp" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
if [[ -z "$RUNNER_TOKEN" || "$RUNNER_TOKEN" != glrt-* ]]; then
  echo "error: failed to create runner: $resp" >&2
  exit 1
fi

echo "==> Deploying gitlab-runner via helmfile"
GITLAB_RUNNER_TOKEN="$RUNNER_TOKEN" helmfile -f "$HELMFILE" apply -l name=gitlab-runner

echo "==> Done. Verify with: kubectl --context $KUBE_CONTEXT -n gitlab-runner get pods"
