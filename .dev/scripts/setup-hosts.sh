#!/usr/bin/env bash
# Adds the *.kind.local hostnames used by .dev/helmfile.yaml.gotmpl's
# HTTPRoutes to /etc/hosts, pointing at 127.0.0.1 (reachable via the
# agentgateway Gateway's hostPort — see CONTRIBUTING.md). Derives the
# hostname list from the helmfile itself so it stays in sync as routes
# are added. Idempotent: replaces its own previously-added line (marked
# with MARKER below) rather than accumulating duplicates.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELMFILE="$REPO_ROOT/.dev/helmfile.yaml.gotmpl"
HOSTS_FILE="/etc/hosts"
MARKER="# added by cigar dev"

hostnames=$(
  grep -oE '^[[:space:]]*-[[:space:]]+[a-zA-Z0-9.-]+\.kind\.local[[:space:]]*$' "$HELMFILE" \
    | sed -E 's/^[[:space:]]*-[[:space:]]+//; s/[[:space:]]*$//' \
    | sort -u
)

if [[ -z "$hostnames" ]]; then
  echo "error: no *.kind.local hostnames found in $HELMFILE" >&2
  exit 1
fi

entry="127.0.0.1 $(tr '\n' ' ' <<< "$hostnames" | sed -E 's/[[:space:]]*$//') $MARKER"

echo "==> Updating $HOSTS_FILE"
# The [ -s ... ] guard stops a grep hiccup from ever swapping in an empty
# file — $HOSTS_FILE always has other content (localhost, comments, ...),
# so an empty temp file means something went wrong, not "nothing to keep".
sudo sh -c "grep -v '$MARKER' '$HOSTS_FILE' > '$HOSTS_FILE.tmp' && [ -s '$HOSTS_FILE.tmp' ] && mv '$HOSTS_FILE.tmp' '$HOSTS_FILE'"
echo "$entry" | sudo tee -a "$HOSTS_FILE" >/dev/null

echo "==> Done: $entry"
