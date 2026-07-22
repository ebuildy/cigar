#!/usr/bin/env bash
# Builds and deploys the bot itself into the dev cluster, wired up against
# the dev GitLab and the dev Prometheus (see .dev/helmfile.yaml.gotmpl),
# then registers its webhook on every existing project (GitLab CE has no
# instance/group-wide pipeline webhook — group-level webhooks are a
# Premium feature and System Hooks don't cover pipeline events — so
# per-project registration is the only way to cover "all projects").
# Idempotent: safe to rerun after code changes (rebuilds+redeploys) or to
# pick up newly created projects (re-registers, skipping ones that already
# have the hook).
set -euo pipefail

KUBE_CONTEXT="${KUBE_CONTEXT:-kind-kind-cluster}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-kind-cluster}"
GITLAB_DEV_URL="${GITLAB_DEV_URL:-http://gitlab.kind.local:9090}"
IMAGE_TAG="${IMAGE_TAG:-cigar:dev}"
TOKEN_NAME="cigar-bot"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELMFILE="$REPO_ROOT/.dev/helmfile.yaml.gotmpl"

echo "==> Building $IMAGE_TAG"
docker build --load -t "$IMAGE_TAG" "$REPO_ROOT"

echo "==> Loading $IMAGE_TAG into kind cluster $KIND_CLUSTER_NAME"
kind load docker-image "$IMAGE_TAG" --name "$KIND_CLUSTER_NAME"

echo "==> Bootstrapping a GitLab API token for the bot ($TOKEN_NAME)"
GITLAB_TOKEN=$(kubectl --context "$KUBE_CONTEXT" -n gitlab exec deploy/gitlab -- gitlab-rails runner "
  user = User.find_by_username!('root')
  user.personal_access_tokens.where(name: '$TOKEN_NAME').destroy_all
  pat = user.personal_access_tokens.create!(
    name: '$TOKEN_NAME',
    scopes: %w[api],
    expires_at: 365.days.from_now
  )
  puts pat.token
" | tail -1)

if [[ -z "$GITLAB_TOKEN" || "$GITLAB_TOKEN" != glpat-* ]]; then
  echo "error: failed to obtain a GitLab API token (got: '$GITLAB_TOKEN')" >&2
  exit 1
fi

API="$GITLAB_DEV_URL/api/v4"

echo "==> Allowing webhooks to reach local/in-cluster addresses"
# GitLab blocks webhooks targeting private/local network addresses by
# default (SSRF protection). The bot's Service is in-cluster (ClusterIP),
# so without this every hook delivery to it would be rejected.
curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  -X PUT "$API/application/settings" \
  --data-urlencode "allow_local_requests_from_web_hooks_and_services=true" \
  -o /dev/null

echo "==> Ensuring a stable WEBHOOK_SECRET"
kubectl --context "$KUBE_CONTEXT" create namespace cigar --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f - >/dev/null
# Reuse the existing WEBHOOK_SECRET so it stays in sync with the token already
# stored on every project's webhook. Minting a fresh secret each run (while
# leaving already-registered hooks untouched) is what caused 401s on redeploy.
WEBHOOK_SECRET=$(kubectl --context "$KUBE_CONTEXT" -n cigar get secret cigar-secrets \
  -o jsonpath='{.data.WEBHOOK_SECRET}' 2>/dev/null | base64 -d || true)
if [[ -z "$WEBHOOK_SECRET" ]]; then
  WEBHOOK_SECRET=$(openssl rand -hex 32)
  echo "    minted a new WEBHOOK_SECRET"
else
  echo "    reusing existing WEBHOOK_SECRET"
fi

echo "==> Writing cigar-secrets"
kubectl --context "$KUBE_CONTEXT" -n cigar create secret generic cigar-secrets \
  --from-literal=WEBHOOK_SECRET="$WEBHOOK_SECRET" \
  --from-literal=GITLAB_TOKEN="$GITLAB_TOKEN" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f - >/dev/null

echo "==> Deploying cigar via helmfile"
helmfile -f "$HELMFILE" apply -l name=cigar

# helmfile won't roll the pod when the image tag is unchanged (cigar:dev) and
# only the externally-managed Secret changed, so a same-tag rebuild or a
# rotated GITLAB_TOKEN/WEBHOOK_SECRET would otherwise keep running on the old
# pod. Force a restart so the freshly-built image and current secrets take hold.
echo "==> Restarting cigar to pick up the new image and secrets"
kubectl --context "$KUBE_CONTEXT" -n cigar rollout restart deploy/cigar
kubectl --context "$KUBE_CONTEXT" -n cigar rollout status deploy/cigar --timeout=180s

echo "==> Registering the webhook on all projects"
WEBHOOK_URL="http://cigar.cigar.svc.cluster.local:8080/webhook"
page=1
registered=0
updated=0
while :; do
  projects=$(curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" "$API/projects?per_page=100&page=$page&simple=true")
  entries=$(printf '%s' "$projects" | python3 -c "
import json,sys
for p in json.load(sys.stdin):
    print(f\"{p['id']}\t{p['path_with_namespace']}\")
")
  [[ -z "$entries" ]] && break

  while IFS=$'\t' read -r id path; do
    existing_hooks=$(curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" "$API/projects/$id/hooks")
    hook_id=$(printf '%s' "$existing_hooks" | python3 -c "
import json,sys
url='$WEBHOOK_URL'
for h in json.load(sys.stdin):
    if h.get('url') == url:
        print(h['id']); break
")
    if [[ -n "$hook_id" ]]; then
      # Refresh the token (and settings) on the existing hook so it always
      # matches the current WEBHOOK_SECRET — otherwise a rotated secret 401s.
      curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
        -X PUT "$API/projects/$id/hooks/$hook_id" \
        --data-urlencode "url=$WEBHOOK_URL" \
        --data-urlencode "token=$WEBHOOK_SECRET" \
        --data-urlencode "pipeline_events=true" \
        --data-urlencode "enable_ssl_verification=false" \
        -o /dev/null
      echo "    [$path] token refreshed"
      updated=$((updated + 1))
      continue
    fi
    curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
      -X POST "$API/projects/$id/hooks" \
      --data-urlencode "url=$WEBHOOK_URL" \
      --data-urlencode "token=$WEBHOOK_SECRET" \
      --data-urlencode "pipeline_events=true" \
      --data-urlencode "enable_ssl_verification=false" \
      -o /dev/null
    echo "    [$path] registered"
    registered=$((registered + 1))
  done <<< "$entries"

  page=$((page + 1))
done

echo "==> Done. Registered on $registered project(s), refreshed $updated existing hook(s)."
echo "    Watch it: kubectl --context $KUBE_CONTEXT -n cigar logs -f deploy/cigar"
