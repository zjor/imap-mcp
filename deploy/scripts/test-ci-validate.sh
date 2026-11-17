#!/usr/bin/env bash
# Tests for ci-validate.sh. Run: make chart-check (or bash this file).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
V="$SCRIPT_DIR/ci-validate.sh"
failures=0

# run <env assignments...> : runs the validator with a clean, valid base env.
run() {
  env -i PATH="$PATH" \
    KUBECONFIG_B64=a2N= INGRESS_HOST=mcp.example.com \
    IMAP_MCP_IMAP_HOSTS=imap.example.com:993 IMAP_MCP_ACCESS_TOKENS=t1,t2 \
    VERSION=v1.2.3 "$@" bash "$V" 2>&1
}

check() { # name, expected exit, expected substring, output, actual exit
  local name="$1" want_rc="$2" want="$3" out="$4" rc="$5"
  if [ "$rc" != "$want_rc" ] || ! grep -qF -- "$want" <<<"$out"; then
    echo "FAIL $name: rc=$rc want=$want_rc, output: $out"; failures=$((failures+1))
  else
    echo "ok   $name"
  fi
}

out=$(run); check "tag push strips v" 0 "tag=1.2.3" "$out" $?
out=$(run VERSION=0.4.0); check "dispatch without v" 0 "tag=0.4.0" "$out" $?
out=$(run VERSION=v1.0.0-rc.1); check "prerelease tag" 0 "tag=1.0.0-rc.1" "$out" $?
out=$(run); check "host is masked" 0 "::add-mask::mcp.example.com" "$out" $?
out=$(run INGRESS_HOST=); check "empty host" 1 "missing secret INGRESS_HOST" "$out" $?
out=$(run KUBECONFIG_B64=); check "empty kubeconfig" 1 "missing secret KUBECONFIG" "$out" $?
out=$(run IMAP_MCP_ACCESS_TOKENS=); check "empty tokens" 1 "missing secret IMAP_MCP_ACCESS_TOKENS" "$out" $?
out=$(run IMAP_MCP_IMAP_HOSTS=); check "empty imap hosts" 1 "missing secret IMAP_MCP_IMAP_HOSTS" "$out" $?
out=$(run INGRESS_HOST=https://mcp.example.com/); check "host with scheme" 1 "INGRESS_HOST must be a bare host name" "$out" $?
out=$(run INGRESS_HOST=mcp.example.com:443); check "host with port" 1 "INGRESS_HOST must be a bare host name" "$out" $?
out=$(run VERSION=main); check "branch name as version" 1 "not a release version" "$out" $?
out=$(run VERSION=); check "empty version" 1 "not a release version" "$out" $?
out=$(run INGRESS_HOST=https://secret.example.com/); \
  if grep -v '::add-mask::' <<<"$out" | grep -qF secret.example.com; then
    echo "FAIL host leaks in error output: $out"; failures=$((failures+1))
  else echo "ok   host not printed in errors"; fi

if [ "$failures" -ne 0 ]; then echo "$failures failed"; exit 1; fi
echo "ci-validate ok"
