#!/usr/bin/env bash
# Checks the deploy inputs in GitHub Actions before anything touches the
# cluster. Reads env: KUBECONFIG_B64, INGRESS_HOST, IMAP_MCP_IMAP_HOSTS,
# IMAP_MCP_ACCESS_TOKENS (GitHub secrets) and VERSION (tag name or the
# workflow_dispatch input). Prints "::add-mask::<host>" and writes
# "tag=<version>" to $GITHUB_OUTPUT (stdout when unset). Never prints secret values.
set -euo pipefail

fail() { echo "deploy check failed: $*" >&2; exit 1; }

for name in KUBECONFIG_B64 INGRESS_HOST IMAP_MCP_IMAP_HOSTS IMAP_MCP_ACCESS_TOKENS; do
  [ -n "${!name:-}" ] || fail "missing secret ${name%_B64}"
done

# The host goes into --set and URLs, so mask it in the log first.
echo "::add-mask::$INGRESS_HOST"
[[ "$INGRESS_HOST" =~ ^[A-Za-z0-9.-]+$ ]] \
  || fail "INGRESS_HOST must be a bare host name (no scheme, port or path)"

tag="${VERSION:-}"
tag="${tag#v}"
[[ "$tag" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
  || fail "VERSION '${VERSION:-}' is not a release version (vX.Y.Z)"

echo "tag=$tag" >> "${GITHUB_OUTPUT:-/dev/stdout}"
