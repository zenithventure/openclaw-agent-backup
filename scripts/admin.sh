#!/usr/bin/env bash
#
# OpenClaw Backup — Admin CLI
#
# Usage:
#   bash admin.sh list [pending|active|suspended]   — list agents
#   bash admin.sh approve <agent_id>                — approve a pending agent
#   bash admin.sh suspend <agent_id>                — suspend an agent
#
set -euo pipefail

BACKUP_SERVICE_URL="${OPENCLAW_BACKUP_URL:-https://6j95borao8.execute-api.us-east-1.amazonaws.com}"
ADMIN_KEY="${ADMIN_API_KEY:-}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { printf '\033[1;34m[admin]\033[0m %s\n' "$*"; }
ok()    { printf '\033[1;32m[admin]\033[0m %s\n' "$*"; }
err()   { printf '\033[1;31m[admin]\033[0m %s\n' "$*" >&2; }
die()   { err "$@"; exit 1; }

admin_curl() {
    local args=(-s)
    if [[ -n "$ADMIN_KEY" ]]; then
        args+=(-H "X-API-Key: $ADMIN_KEY")
    fi
    curl "${args[@]}" "$@"
}

usage() {
    cat <<EOF
Usage: bash admin.sh <command> [args]

Commands:
  list [status]       List agents (optional: pending, active, suspended)
  approve <agent_id>  Approve a pending agent
  suspend <agent_id>  Suspend an agent

Environment:
  OPENCLAW_BACKUP_URL  Service URL (default: https://6j95borao8.execute-api.us-east-1.amazonaws.com)
  ADMIN_API_KEY        Admin API key for X-API-Key header
EOF
    exit 1
}

# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------

cmd_list() {
    local status="${1:-}"
    local url="$BACKUP_SERVICE_URL/v1/admin/agents"
    if [[ -n "$status" ]]; then
        url="$url?status=$status"
    fi

    local resp
    resp=$(admin_curl "$url")

    local count
    count=$(echo "$resp" | jq 'length')

    if [[ "$count" == "0" ]]; then
        info "No agents found${status:+ with status '$status'}"
        return
    fi

    echo "$resp" | jq -r '
        ["AGENT_ID", "NAME", "HOSTNAME", "STATUS", "CREATED"],
        (.[] | [.agent_id, .name, .hostname, .status, .created_at]) |
        @tsv
    ' | column -t -s $'\t'

    echo ""
    info "$count agent(s)"
}

cmd_approve() {
    local agent_id="${1:-}"
    [[ -n "$agent_id" ]] || die "Usage: admin.sh approve <agent_id>"

    local resp
    resp=$(admin_curl -X POST "$BACKUP_SERVICE_URL/v1/admin/agents/$agent_id/approve")

    local status
    status=$(echo "$resp" | jq -r '.status // .error // "unknown"')

    if [[ "$status" == "active" ]]; then
        ok "Agent $agent_id approved (status: active)"
    else
        die "Failed to approve $agent_id: $status"
    fi
}

cmd_suspend() {
    local agent_id="${1:-}"
    [[ -n "$agent_id" ]] || die "Usage: admin.sh suspend <agent_id>"

    local resp
    resp=$(admin_curl -X POST "$BACKUP_SERVICE_URL/v1/admin/agents/$agent_id/suspend")

    local status
    status=$(echo "$resp" | jq -r '.status // .error // "unknown"')

    if [[ "$status" == "suspended" ]]; then
        ok "Agent $agent_id suspended"
    else
        die "Failed to suspend $agent_id: $status"
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
[[ $# -ge 1 ]] || usage

case "$1" in
    list)    cmd_list "${2:-}" ;;
    approve) cmd_approve "${2:-}" ;;
    suspend) cmd_suspend "${2:-}" ;;
    *)       usage ;;
esac
