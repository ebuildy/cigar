#!/usr/bin/env bash
# Seeds a "load test" project into the dev GitLab instance and opens a merge
# request whose pipeline exercises cigar's resource report (CPU throttling,
# memory, network — see .dev/fixtures/load-test.gitlab-ci.yml).
#
# It:
#   1. bootstraps a root personal access token (api scope),
#   2. creates the project (idempotent — reused if it already exists),
#   3. commits .gitlab-ci.yml onto a fresh branch via the GitLab Files API,
#   4. opens a merge request from that branch, which triggers the pipeline.
#
# Each run uses a unique branch, so rerunning just opens a new MR + pipeline.
set -euo pipefail

KUBE_CONTEXT="${KUBE_CONTEXT:-kind-kind-cluster}"
GITLAB_DEV_URL="${GITLAB_DEV_URL:-http://gitlab.kind.local:9090}"
GITLAB_NAMESPACE="${GITLAB_NAMESPACE:-root}"
PROJECT_NAME="${PROJECT_NAME:-ci-load-test}"
PROJECT_VISIBILITY="${PROJECT_VISIBILITY:-public}"
TOKEN_NAME="dev-bootstrap"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CI_FILE="$REPO_ROOT/.dev/fixtures/load-test.gitlab-ci.yml"
FEATURE_BRANCH="ci-load-test-$(date +%s)"

[[ -f "$CI_FILE" ]] || { echo "error: missing $CI_FILE" >&2; exit 1; }

# json <jq-ish python expr> reads stdin JSON as `d` and prints the expression.
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

echo "==> Bootstrapping a root personal access token ($TOKEN_NAME)"
TOKEN=$(kubectl --context "$KUBE_CONTEXT" -n gitlab exec deploy/gitlab -- gitlab-rails runner "
  user = User.find_by_username!('root')
  user.personal_access_tokens.where(name: '$TOKEN_NAME').destroy_all
  pat = user.personal_access_tokens.create!(
    name: '$TOKEN_NAME',
    scopes: %w[api],
    expires_at: 365.days.from_now
  )
  puts pat.token
" | tail -1)

if [[ -z "$TOKEN" || "$TOKEN" != glpat-* ]]; then
  echo "error: failed to obtain a personal access token (got: '$TOKEN')" >&2
  exit 1
fi

API="$GITLAB_DEV_URL/api/v4"
PROJECT_PATH="$GITLAB_NAMESPACE/$PROJECT_NAME"
# URL-encoded "namespace/project" for the :id path segment.
PROJECT_ID_ENC="${GITLAB_NAMESPACE}%2F${PROJECT_NAME}"

echo "==> Ensuring project $PROJECT_PATH exists"
create_body=$(mktemp)
trap 'rm -f "$create_body"' EXIT
status=$(curl -s -o "$create_body" -w '%{http_code}' \
  -H "PRIVATE-TOKEN: $TOKEN" \
  -X POST "$API/projects" \
  --data-urlencode "name=$PROJECT_NAME" \
  --data-urlencode "visibility=$PROJECT_VISIBILITY" \
  --data-urlencode "initialize_with_readme=true")

case "$status" in
  201) echo "    created" ;;
  400)
    if grep -q "has already been taken" "$create_body"; then
      echo "    already exists, reusing it"
    else
      echo "error: project creation failed (400): $(cat "$create_body")" >&2
      exit 1
    fi
    ;;
  *)
    echo "error: project creation failed ($status): $(cat "$create_body")" >&2
    exit 1
    ;;
esac

echo "==> Resolving project id and default branch"
project=$(curl -sf -H "PRIVATE-TOKEN: $TOKEN" "$API/projects/$PROJECT_ID_ENC")
PROJECT_ID=$(printf '%s' "$project" | json "d['id']")
DEFAULT_BRANCH=$(printf '%s' "$project" | json "d['default_branch'] or 'main'")
echo "    id=$PROJECT_ID default_branch=$DEFAULT_BRANCH"

echo "==> Creating branch $FEATURE_BRANCH from $DEFAULT_BRANCH"
curl -sf -H "PRIVATE-TOKEN: $TOKEN" \
  -X POST "$API/projects/$PROJECT_ID/repository/branches" \
  --data-urlencode "branch=$FEATURE_BRANCH" \
  --data-urlencode "ref=$DEFAULT_BRANCH" \
  -o /dev/null

echo "==> Committing .gitlab-ci.yml onto $FEATURE_BRANCH via the Files API"
curl -sf -H "PRIVATE-TOKEN: $TOKEN" \
  -X POST "$API/projects/$PROJECT_ID/repository/files/.gitlab-ci.yml" \
  --data-urlencode "branch=$FEATURE_BRANCH" \
  --data-urlencode "commit_message=Add load-test pipeline" \
  --data-urlencode "content@$CI_FILE" \
  -o /dev/null

echo "==> Opening merge request $FEATURE_BRANCH -> $DEFAULT_BRANCH"
mr=$(curl -sf -H "PRIVATE-TOKEN: $TOKEN" \
  -X POST "$API/projects/$PROJECT_ID/merge_requests" \
  --data-urlencode "source_branch=$FEATURE_BRANCH" \
  --data-urlencode "target_branch=$DEFAULT_BRANCH" \
  --data-urlencode "title=Load test: CPU throttle, memory, network" \
  --data-urlencode "remove_source_branch=true")

MR_URL=$(printf '%s' "$mr" | json "d['web_url']")
MR_IID=$(printf '%s' "$mr" | json "d['iid']")

echo "==> Done."
echo "    Merge request !$MR_IID: $MR_URL"
echo "    Pipeline:            $GITLAB_DEV_URL/$PROJECT_PATH/-/pipelines"
echo "    Once it finishes, cigar should post its report as an MR comment."
