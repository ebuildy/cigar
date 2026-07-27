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

echo "==> Writing cigar-secrets (GITLAB_TOKEN only)"
kubectl --context "$KUBE_CONTEXT" create namespace cigar --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f - >/dev/null
# Only the PAT lives here: gitlab-rails is the only thing that can issue one, so
# it cannot be generated. WEBHOOK_SIGNING_TOKEN and COMMANDS_SIGNING_KEY are
# minted in-cluster by the cigar-password ClusterGenerator through ESO — see the
# cigar release in the helmfile — and read back below.
kubectl --context "$KUBE_CONTEXT" -n cigar create secret generic cigar-secrets \
  --from-literal=GITLAB_TOKEN="$GITLAB_TOKEN" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f - >/dev/null

echo "==> Deploying cigar via helmfile"
helmfile -f "$HELMFILE" apply -l name=cigar

echo "==> Reading the ESO-generated WEBHOOK_SIGNING_TOKEN"
# The chart renders one ExternalSecret per externalSecrets entry; ESO defaults
# the Secret it manages to the same name, so the "keys" entry lands in
# cigar-keys. ESO fills it moments after the release is applied, so poll
# briefly rather than assume it is already there.
SIGNING_SECRET="cigar-keys"
WEBHOOK_SIGNING_TOKEN=""
for _ in $(seq 1 30); do
  WEBHOOK_SIGNING_TOKEN=$(kubectl --context "$KUBE_CONTEXT" -n cigar get secret "$SIGNING_SECRET" \
    -o jsonpath='{.data.WEBHOOK_SIGNING_TOKEN}' 2>/dev/null | base64 -d || true)
  [[ -n "$WEBHOOK_SIGNING_TOKEN" ]] && break
  sleep 2
done
if [[ -z "$WEBHOOK_SIGNING_TOKEN" ]]; then
  echo "error: ESO did not materialise $SIGNING_SECRET; check the ExternalSecret:" >&2
  echo "  kubectl --context $KUBE_CONTEXT -n cigar describe externalsecret $SIGNING_SECRET" >&2
  exit 1
fi
# The chart's ESO template should have produced whsec_<base64 of 32 bytes>.
if [[ ! "$WEBHOOK_SIGNING_TOKEN" =~ ^whsec_[A-Za-z0-9+/]{43}=$ ]]; then
  echo "error: generated WEBHOOK_SIGNING_TOKEN is not whsec_<base64 of a 32-byte key>." >&2
  echo "       Check that the cigar-password generator emits 32 raw characters." >&2
  exit 1
fi
echo "    read a ${#WEBHOOK_SIGNING_TOKEN}-char token from $SIGNING_SECRET"

# helmfile won't roll the pod when the image tag is unchanged (cigar:dev) and
# only the externally-managed Secret changed, so a same-tag rebuild or a
# rotated GITLAB_TOKEN would otherwise keep running on the old pod. Force a
# restart so the freshly-built image and current secrets take hold.
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
      # Refresh the signing token (and settings) on the existing hook so it
      # always matches the current WEBHOOK_SIGNING_TOKEN — otherwise a rotated
      # token makes every signature verification 401.
      curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
        -X PUT "$API/projects/$id/hooks/$hook_id" \
        --data-urlencode "url=$WEBHOOK_URL" \
        --data-urlencode "signing_token=$WEBHOOK_SIGNING_TOKEN" \
        --data-urlencode "pipeline_events=true" \
        --data-urlencode "note_events=true" \
        --data-urlencode "enable_ssl_verification=false" \
        -o /dev/null
      echo "    [$path] signing token refreshed"
      updated=$((updated + 1))
      continue
    fi
    curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
      -X POST "$API/projects/$id/hooks" \
      --data-urlencode "name=cigar" \
      --data-urlencode "url=$WEBHOOK_URL" \
      --data-urlencode "signing_token=$WEBHOOK_SIGNING_TOKEN" \
      --data-urlencode "pipeline_events=true" \
      --data-urlencode "note_events=true" \
      --data-urlencode "enable_ssl_verification=false" \
      -o /dev/null
    echo "    [$path] registered"
    registered=$((registered + 1))
  done <<< "$entries"

  page=$((page + 1))
done

echo "==> Done. Registered on $registered project(s), refreshed $updated existing hook(s)."
echo "    Watch it: kubectl --context $KUBE_CONTEXT -n cigar logs -f deploy/cigar"
