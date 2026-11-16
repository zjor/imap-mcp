#!/usr/bin/env bash
# Prints the imap-mcp request headers as JSON, for the Claude Code
# "headersHelper" setting. Values come from the environment or an env
# file, by default ./.env.local (override with IMAP_MCP_ENV_FILE). Needs jq.
#
# Required variables: IMAP_MCP_TOKEN, IMAP_USER, IMAP_PASSWORD.
# Optional: IMAP_HOST (only when the server allows more than one host).
set -euo pipefail

env_file="${IMAP_MCP_ENV_FILE:-./.env.local}"
IMAP_MCP_TOKEN="${IMAP_MCP_TOKEN:-}" IMAP_USER="${IMAP_USER:-}"
IMAP_PASSWORD="${IMAP_PASSWORD:-}" IMAP_HOST="${IMAP_HOST:-}"

# Read KEY=VALUE lines the same way the server reads .env.local: no shell
# expansion, an optional "export " prefix, optional matching quotes. The
# file is never executed. Only the variables below are taken from it, and
# a variable already set in the environment wins.
[[ -f "$env_file" ]] && while IFS= read -r line || [[ -n "$line" ]]; do
	line="${line#"${line%%[![:space:]]*}"}"
	line="${line%"${line##*[![:space:]]}"}"
	[[ -z "$line" || "$line" == \#* || "$line" != *=* ]] && continue
	line="${line#export }"
	key="${line%%=*}"
	key="${key%"${key##*[![:space:]]}"}"
	val="${line#*=}"
	val="${val#"${val%%[![:space:]]*}"}"
	if [[ ${#val} -ge 2 && ( "$val" == \"*\" || "$val" == \'*\' ) ]]; then
		val="${val:1:${#val}-2}"
	fi
	case "$key" in
	IMAP_MCP_TOKEN | IMAP_USER | IMAP_PASSWORD | IMAP_HOST)
		[[ -z "${!key}" ]] && printf -v "$key" '%s' "$val"
		;;
	esac
done <"$env_file"

for v in IMAP_MCP_TOKEN IMAP_USER IMAP_PASSWORD; do
	if [[ -z "${!v}" ]]; then
		echo "imap-mcp-headers: $v is not set (environment or $env_file)" >&2
		exit 1
	fi
done

jq -n \
	--arg auth "Bearer $IMAP_MCP_TOKEN" \
	--arg user "$IMAP_USER" \
	--arg pass "$IMAP_PASSWORD" \
	--arg host "${IMAP_HOST:-}" \
	'{"Authorization": $auth, "X-IMAP-Username": $user, "X-IMAP-Password": $pass}
	 + (if $host != "" then {"X-IMAP-Host": $host} else {} end)'
