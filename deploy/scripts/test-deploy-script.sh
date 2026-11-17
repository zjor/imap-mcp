#!/usr/bin/env bash
# Tests deploy-with-helm.sh with a fake helm that prints its arguments.
# Run: make chart-check
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
printf '#!/bin/sh\necho "helm $*"\n' > "$TMP/helm"; chmod +x "$TMP/helm"
failures=0

run() { env -i PATH="$TMP:$PATH" ENV_FILE="$TMP/none" INGRESS_HOST=mcp.example.com "$@" \
  bash "$SCRIPT_DIR/deploy-with-helm.sh" v1.2.3 2>&1; }
has() {
  if grep -qF -- "$2" <<<"$3"; then echo "ok   $1"; else echo "FAIL $1: $3"; failures=$((failures+1)); fi
}
lacks() {
  if grep -qF -- "$2" <<<"$3"; then echo "FAIL $1: $3"; failures=$((failures+1)); else echo "ok   $1"; fi
}

out=$(run IMAP_MCP_DRAFTS_FOLDER=Drafts IMAP_MCP_SENT_FOLDER=Sent)
has "drafts folder passed" "--set-string env.IMAP_MCP_DRAFTS_FOLDER=Drafts" "$out"
has "sent folder passed" "--set-string env.IMAP_MCP_SENT_FOLDER=Sent" "$out"
has "tag without v" "--set image.tag=1.2.3" "$out"

out=$(run)
lacks "no drafts folder when unset" "IMAP_MCP_DRAFTS_FOLDER" "$out"
lacks "no sent folder when unset" "IMAP_MCP_SENT_FOLDER" "$out"

if [ "$failures" -ne 0 ]; then echo "$failures failed"; exit 1; fi
echo "deploy script ok"
