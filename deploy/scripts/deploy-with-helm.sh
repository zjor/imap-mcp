#!/usr/bin/env bash
# Manual fallback: deploys a released version with Helm.
#   deploy/scripts/deploy-with-helm.sh 0.1.0
# The version must exist as ghcr.io/zjor/imap-mcp:<version>. A leading "v" is
# stripped. INGRESS_HOST comes from deploy/.env or the environment.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/env.sh
. "$SCRIPT_DIR/env.sh"
CHART="$SCRIPT_DIR/../chart"
NS="imap-mcp"
RELEASE="imap-mcp"
KUBE_CONTEXT="${KUBE_CONTEXT:-midgard}"   # never use whatever context is current
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/../.env}"

VERSION="${1:-}"
VERSION="${VERSION#v}"
[ -n "$VERSION" ] || { echo "usage: $0 <version>" >&2; exit 1; }

load_env "$ENV_FILE"
require INGRESS_HOST

helm --kube-context "$KUBE_CONTEXT" upgrade --install "$RELEASE" "$CHART" \
  --namespace "$NS" --create-namespace \
  --set image.tag="$VERSION" --set ingress.host="$INGRESS_HOST" \
  --atomic --timeout 5m

echo "Deployed $RELEASE $VERSION to $NS on $KUBE_CONTEXT."
