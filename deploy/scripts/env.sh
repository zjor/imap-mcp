# shellcheck shell=bash
# Sourced by the deploy scripts. Reads KEY=VALUE lines from $ENV_FILE
# (default deploy/.env) without shell expansion. Real environment variables
# win over the file. A missing file is fine when the environment has the values.
load_env() {
  local file="$1" line key val
  [ -f "$file" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|\#*) continue ;; esac
    key="${line%%=*}"; val="${line#*=}"
    case "$key" in INGRESS_HOST|IMAP_MCP_IMAP_HOSTS|IMAP_MCP_ACCESS_TOKENS) ;; *) continue ;; esac
    [ -n "${!key:-}" ] || printf -v "$key" '%s' "$val"
  done < "$file"
}

require() {
  local name
  for name in "$@"; do
    [ -n "${!name:-}" ] || { echo "$name is empty (set it in the env file or the environment)" >&2; exit 1; }
  done
}
