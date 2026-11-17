#!/usr/bin/env bash
# Manual fallback: writes the imap-mcp-secrets Secret from deploy/.env.
# CI does the same from GitHub secrets on every deploy.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/env.sh
. "$SCRIPT_DIR/env.sh"
NS="imap-mcp"
SECRET="imap-mcp-secrets"
KUBE_CONTEXT="${KUBE_CONTEXT:-midgard}"   # never use whatever context is current
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/../.env}"

load_env "$ENV_FILE"
require IMAP_MCP_IMAP_HOSTS IMAP_MCP_ACCESS_TOKENS

kubectl --context "$KUBE_CONTEXT" create namespace "$NS" --dry-run=client -o yaml \
  | kubectl --context "$KUBE_CONTEXT" apply -f -

kubectl --context "$KUBE_CONTEXT" -n "$NS" create secret generic "$SECRET" \
  --from-literal=IMAP_MCP_IMAP_HOSTS="$IMAP_MCP_IMAP_HOSTS" \
  --from-literal=IMAP_MCP_ACCESS_TOKENS="$IMAP_MCP_ACCESS_TOKENS" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f -

echo "Secret $SECRET applied in $NS."
# Running pods read the Secret only at start.
kubectl --context "$KUBE_CONTEXT" -n "$NS" rollout restart deployment/imap-mcp 2>/dev/null || true
