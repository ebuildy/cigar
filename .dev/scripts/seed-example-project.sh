#!/usr/bin/env bash
# Mirrors an example project into the dev GitLab instance (see
# .dev/helmfile.yaml.gotmpl), so there's a real project/pipeline to point
# the bot at instead of only the e2e suite's mocks. Idempotent: safe to
# rerun to pick up upstream changes.
set -euo pipefail

KUBE_CONTEXT="${KUBE_CONTEXT:-kind-kind-cluster}"
GITLAB_DEV_URL="${GITLAB_DEV_URL:-http://gitlab.kind.local:9090}"
GITLAB_NAMESPACE="${GITLAB_NAMESPACE:-root}"
PROJECT_NAME="${PROJECT_NAME:-sample-java-project}"
PROJECT_VISIBILITY="${PROJECT_VISIBILITY:-public}"
SOURCE_REPO="${SOURCE_REPO:-https://gitlab.com/Wjdi/sample-java-project.git}"
TOKEN_NAME="dev-bootstrap"

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

echo "==> Bootstrapping a root personal access token ($TOKEN_NAME)"
TOKEN=$(kubectl --context "$KUBE_CONTEXT" -n gitlab exec deploy/gitlab -- gitlab-rails runner "
  user = User.find_by_username!('root')
  user.personal_access_tokens.where(name: '$TOKEN_NAME').destroy_all
  pat = user.personal_access_tokens.create!(
    name: '$TOKEN_NAME',
    scopes: %w[api write_repository],
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

echo "==> Ensuring project $PROJECT_PATH exists"
create_body="$workdir/create-response.json"
status=$(curl -s -o "$create_body" -w '%{http_code}' \
  -H "PRIVATE-TOKEN: $TOKEN" \
  -X POST "$API/projects" \
  --data-urlencode "name=$PROJECT_NAME" \
  --data-urlencode "visibility=$PROJECT_VISIBILITY")

case "$status" in
  201) echo "    created" ;;
  400)
    if grep -q "has already been taken" "$create_body"; then
      echo "    already exists, will push updates over it"
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

echo "==> Mirroring $SOURCE_REPO -> $GITLAB_DEV_URL/$PROJECT_PATH"
git clone --bare -q "$SOURCE_REPO" "$workdir/src.git"
git -C "$workdir/src.git" push --mirror \
  "$(printf '%s' "$GITLAB_DEV_URL" | sed "s#://#://root:${TOKEN}@#")/$PROJECT_PATH.git"

echo "==> Done: $GITLAB_DEV_URL/$PROJECT_PATH"
