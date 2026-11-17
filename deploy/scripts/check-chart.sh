#!/usr/bin/env bash
# Checks the Helm chart without a cluster: helm lint, then helm template with
# test values, then greps the rendered manifests. Run: make chart-check
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART="$SCRIPT_DIR/../chart"
HOST="mcp.test.invalid"
TAG="1.2.3"

fail() { echo "chart check failed: $*" >&2; exit 1; }

helm lint "$CHART" --set ingress.host="$HOST" --set image.tag="$TAG" >/dev/null \
  || fail "helm lint"

out="$(helm template imap-mcp "$CHART" --namespace imap-mcp \
  --set ingress.host="$HOST" --set image.tag="$TAG")" || fail "helm template"

expect() {
  grep -qF -- "$1" <<<"$out" || fail "rendered chart has no line: $1"
}

expect 'image: "ghcr.io/zjor/imap-mcp:1.2.3"'
expect 'value: "https://mcp.test.invalid"'
grep -A1 -F 'name: IMAP_MCP_PUBLIC_URL' <<<"$out" | grep -qF 'https://mcp.test.invalid' \
  || fail "IMAP_MCP_PUBLIC_URL is not https://$HOST"
grep -A1 -F 'name: IMAP_MCP_INSECURE_HTTP' <<<"$out" | grep -qF '"true"' \
  || fail "IMAP_MCP_INSECURE_HTTP is not \"true\""
grep -A1 -F 'secretRef:' <<<"$out" | grep -qF 'name: imap-mcp-secrets' \
  || fail "envFrom secretRef imap-mcp-secrets missing"
expect 'readOnlyRootFilesystem: true'
expect 'runAsUser: 65532'
expect 'ingressClassName: nginx'
expect 'cert-manager.io/cluster-issuer: letsencrypt-prod'
expect 'nginx.ingress.kubernetes.io/proxy-body-size: 30m'
expect 'nginx.ingress.kubernetes.io/proxy-buffering: "off"'
expect 'nginx.ingress.kubernetes.io/proxy-request-buffering: "off"'
expect 'nginx.ingress.kubernetes.io/proxy-read-timeout: "600"'
expect 'nginx.ingress.kubernetes.io/proxy-send-timeout: "600"'
expect "- $HOST"

# Missing required values must fail the render with a clear message.
if err="$(helm template imap-mcp "$CHART" --set image.tag="$TAG" 2>&1)"; then
  fail "render without ingress.host succeeded"
fi
grep -qF 'ingress.host is required' <<<"$err" || fail "no 'ingress.host is required' message: $err"

if err="$(helm template imap-mcp "$CHART" --set ingress.host="$HOST" 2>&1)"; then
  fail "render without image.tag succeeded"
fi
grep -qF 'image.tag is required' <<<"$err" || fail "no 'image.tag is required' message: $err"

echo "chart ok"
