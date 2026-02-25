#!/usr/bin/env bash
#
# OpenClaw Backup Skill — Uninstall
#
# Removes the daily scheduler. Does NOT delete the master key or remote backups.
#
# Usage:
#   bash uninstall.sh               # remove scheduler only
#   bash uninstall.sh --purge       # also remove local state (keys, tokens)
#   bash uninstall.sh --purge-all   # also delete remote backups
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths & defaults
# ---------------------------------------------------------------------------
OPENCLAW_DIR="${OPENCLAW_DIR:-$HOME/.openclaw}"
STATE_DIR="$OPENCLAW_DIR/skills/backup/.state"
BACKUP_SERVICE_URL="${OPENCLAW_BACKUP_URL:-https://agentbackup.zenithstudio.app}"
LABEL="ai.openclaw.backup"
PURGE=0
PURGE_ALL=0

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { printf '\033[1;34m[backup]\033[0m %s\n' "$*"; }
ok()    { printf '\033[1;32m[backup]\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m[backup]\033[0m %s\n' "$*"; }
err()   { printf '\033[1;31m[backup]\033[0m %s\n' "$*" >&2; }
die()   { err "$@"; exit 1; }

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --purge)     PURGE=1; shift ;;
        --purge-all) PURGE=1; PURGE_ALL=1; shift ;;
        *)           die "Unknown option: $1" ;;
    esac
done

# ---------------------------------------------------------------------------
# Remove scheduler
# ---------------------------------------------------------------------------
OS="$(uname -s)"

case "$OS" in
    Darwin)
        PLIST_FILE="$HOME/Library/LaunchAgents/$LABEL.plist"
        if [[ -f "$PLIST_FILE" ]]; then
            launchctl unload "$PLIST_FILE" 2>/dev/null || true
            rm -f "$PLIST_FILE"
            ok "Removed macOS LaunchAgent"
        else
            info "No LaunchAgent found"
        fi
        ;;

    Linux)
        # Try systemd first
        SYSTEMD_DIR="$HOME/.config/systemd/user"
        if [[ -f "$SYSTEMD_DIR/$LABEL.timer" ]]; then
            systemctl --user disable --now "$LABEL.timer" 2>/dev/null || true
            rm -f "$SYSTEMD_DIR/$LABEL.timer" "$SYSTEMD_DIR/$LABEL.service"
            systemctl --user daemon-reload 2>/dev/null || true
            ok "Removed systemd timer"
        fi

        # Also clean cron
        if crontab -l 2>/dev/null | grep -q "$LABEL"; then
            crontab -l 2>/dev/null | grep -v "$LABEL" | crontab -
            ok "Removed cron entry"
        fi
        ;;
esac

# ---------------------------------------------------------------------------
# Purge remote backups
# ---------------------------------------------------------------------------
if [[ $PURGE_ALL -eq 1 ]]; then
    warn "Deleting ALL remote backups..."
    if [[ -f "$STATE_DIR/agent.token" ]]; then
        TOKEN="$(cat "$STATE_DIR/agent.token")"
        HTTP_STATUS=$(curl -sf -o /dev/null -w "%{http_code}" \
            -X DELETE \
            -H "Authorization: Bearer $TOKEN" \
            "$BACKUP_SERVICE_URL/v1/backups" 2>/dev/null) || true

        if [[ "${HTTP_STATUS:-0}" =~ ^2 ]]; then
            ok "Remote backups deleted"
        else
            err "Failed to delete remote backups (HTTP $HTTP_STATUS). Delete manually via the service."
        fi
    else
        err "No agent token — cannot delete remote backups"
    fi
fi

# ---------------------------------------------------------------------------
# Purge local state
# ---------------------------------------------------------------------------
if [[ $PURGE -eq 1 ]]; then
    if [[ -f "$STATE_DIR/master.key" ]]; then
        echo ""
        warn "============================================================"
        warn "  About to delete the master encryption key."
        warn "  All existing backups will become UNRECOVERABLE."
        warn "============================================================"
        echo ""
        warn "Key location: $STATE_DIR/master.key"
        warn "Make sure you have saved the key elsewhere if needed."
        echo ""
    fi

    rm -rf "$STATE_DIR"
    ok "Local state purged"
else
    info "Local state preserved at $STATE_DIR"
    info "  Master key: $STATE_DIR/master.key"
    info "  Agent token: $STATE_DIR/agent.token"
    info "  Use --purge to remove local state"
fi

echo ""
ok "Uninstall complete. Backup scheduler removed."
