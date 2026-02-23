#!/usr/bin/env bash
#
# OpenClaw Backup Skill — Setup
#
# Generates encryption keys, registers with the backup service,
# installs a daily scheduler, and runs the first backup.
#
# Usage: bash setup.sh [--service-url URL]
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths & defaults
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPENCLAW_DIR="${OPENCLAW_DIR:-$HOME/.openclaw}"
STATE_DIR="$OPENCLAW_DIR/skills/backup/.state"
BACKUP_SERVICE_URL="${OPENCLAW_BACKUP_URL:-https://backup.openclaw.ai}"
SCHEDULE_HOUR="${OPENCLAW_BACKUP_HOUR:-3}"
LABEL="ai.openclaw.backup"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { printf '\033[1;34m[backup]\033[0m %s\n' "$*"; }
ok()    { printf '\033[1;32m[backup]\033[0m %s\n' "$*"; }
err()   { printf '\033[1;31m[backup]\033[0m %s\n' "$*" >&2; }
die()   { err "$@"; exit 1; }

has_cmd() { command -v "$1" &>/dev/null; }

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --service-url) BACKUP_SERVICE_URL="$2"; shift 2 ;;
        --schedule-hour) SCHEDULE_HOUR="$2"; shift 2 ;;
        *) die "Unknown option: $1" ;;
    esac
done

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------
info "Running preflight checks..."

[[ -d "$OPENCLAW_DIR" ]] || die "OpenClaw directory not found at $OPENCLAW_DIR"

for cmd in curl tar jq; do
    has_cmd "$cmd" || die "Required command not found: $cmd"
done

# Determine encryption tool
if has_cmd age; then
    ENCRYPT_TOOL="age"
elif has_cmd openssl; then
    ENCRYPT_TOOL="openssl"
    info "age not found, falling back to openssl"
else
    die "No encryption tool found. Install age (recommended) or openssl."
fi

ok "Preflight checks passed (encryption: $ENCRYPT_TOOL)"

# ---------------------------------------------------------------------------
# Create state directory
# ---------------------------------------------------------------------------
mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"

# Save encryption tool preference
echo "$ENCRYPT_TOOL" > "$STATE_DIR/encrypt-tool"

# ---------------------------------------------------------------------------
# Generate master encryption key
# ---------------------------------------------------------------------------
if [[ -f "$STATE_DIR/master.key" ]]; then
    info "Master key already exists, skipping generation"
else
    info "Generating master encryption key..."

    if [[ "$ENCRYPT_TOOL" == "age" ]]; then
        age-keygen 2>"$STATE_DIR/master.key.pub" | head -n 2 > "$STATE_DIR/master.key"
        chmod 600 "$STATE_DIR/master.key"
        chmod 600 "$STATE_DIR/master.key.pub"

        # Extract the public key (recipient) for display and registration
        AGE_RECIPIENT=$(grep '^age1' "$STATE_DIR/master.key.pub" || grep 'public key:' "$STATE_DIR/master.key.pub" | awk '{print $NF}')
        if [[ -z "$AGE_RECIPIENT" ]]; then
            # age-keygen outputs the public key as a comment in the secret key file
            AGE_RECIPIENT=$(grep '^# public key:' "$STATE_DIR/master.key" | awk '{print $NF}')
        fi
        echo "$AGE_RECIPIENT" > "$STATE_DIR/recipient.txt"
    else
        # openssl: generate a random 256-bit key
        openssl rand -hex 32 > "$STATE_DIR/master.key"
        chmod 600 "$STATE_DIR/master.key"
    fi

    ok "Master key generated"

    # Display recovery key warning
    echo ""
    echo "============================================================"
    echo "  RECOVERY KEY — SAVE THIS SOMEWHERE SAFE"
    echo "============================================================"
    if [[ "$ENCRYPT_TOOL" == "age" ]]; then
        echo "  Public key (recipient): $AGE_RECIPIENT"
        echo ""
        echo "  The secret key is stored at:"
        echo "    $STATE_DIR/master.key"
    else
        echo "  The symmetric key is stored at:"
        echo "    $STATE_DIR/master.key"
    fi
    echo ""
    echo "  If this key is lost, your backups are UNRECOVERABLE."
    echo "  Copy the key file to a secure offline location now."
    echo "============================================================"
    echo ""
fi

# ---------------------------------------------------------------------------
# Collect agent identity
# ---------------------------------------------------------------------------
info "Collecting agent identity..."

AGENT_HOSTNAME="$(hostname -s 2>/dev/null || echo 'unknown')"
AGENT_OS="$(uname -s)"
AGENT_ARCH="$(uname -m)"
OPENCLAW_VERSION="unknown"
if has_cmd openclaw; then
    OPENCLAW_VERSION="$(openclaw --version 2>/dev/null || echo 'unknown')"
fi

# Try to extract agent name from workspace
AGENT_NAME="default"
if [[ -f "$OPENCLAW_DIR/workspace/IDENTITY.md" ]]; then
    AGENT_NAME="$(head -1 "$OPENCLAW_DIR/workspace/IDENTITY.md" | sed 's/^#* *//' | tr -d '\n')"
fi
[[ -z "$AGENT_NAME" ]] && AGENT_NAME="agent-$AGENT_HOSTNAME"

# Generate a stable machine fingerprint (hash of hostname + OS + user)
MACHINE_FINGERPRINT="$(printf '%s:%s:%s' "$AGENT_HOSTNAME" "$AGENT_OS" "$(whoami)" | shasum -a 256 | cut -d' ' -f1)"

# ---------------------------------------------------------------------------
# Register with backup service
# ---------------------------------------------------------------------------
if [[ -f "$STATE_DIR/agent.token" ]]; then
    info "Agent already registered, skipping registration"
else
    info "Registering with backup service at $BACKUP_SERVICE_URL ..."

    REGISTER_PAYLOAD=$(jq -n \
        --arg name "$AGENT_NAME" \
        --arg hostname "$AGENT_HOSTNAME" \
        --arg os "$AGENT_OS" \
        --arg arch "$AGENT_ARCH" \
        --arg version "$OPENCLAW_VERSION" \
        --arg fingerprint "$MACHINE_FINGERPRINT" \
        --arg encrypt_tool "$ENCRYPT_TOOL" \
        --arg public_key "$(cat "$STATE_DIR/recipient.txt" 2>/dev/null || echo '')" \
        '{
            agent_name: $name,
            hostname: $hostname,
            os: $os,
            arch: $arch,
            openclaw_version: $version,
            machine_fingerprint: $fingerprint,
            encrypt_tool: $encrypt_tool,
            public_key: $public_key
        }')

    REGISTER_RESPONSE=$(curl -sf \
        -X POST \
        -H "Content-Type: application/json" \
        -d "$REGISTER_PAYLOAD" \
        "$BACKUP_SERVICE_URL/v1/agents/register" 2>&1) \
        || die "Registration failed. Is the backup service reachable at $BACKUP_SERVICE_URL?"

    # Parse response
    AGENT_ID=$(echo "$REGISTER_RESPONSE" | jq -r '.agent_id // empty')
    AGENT_TOKEN=$(echo "$REGISTER_RESPONSE" | jq -r '.token // empty')
    QUOTA_MB=$(echo "$REGISTER_RESPONSE" | jq -r '.quota_mb // "500"')

    [[ -n "$AGENT_ID" ]]    || die "Registration response missing agent_id"
    [[ -n "$AGENT_TOKEN" ]] || die "Registration response missing token"

    echo "$AGENT_TOKEN" > "$STATE_DIR/agent.token"
    chmod 600 "$STATE_DIR/agent.token"

    echo "$AGENT_ID" > "$STATE_DIR/agent.id"

    ok "Registered as $AGENT_ID (quota: ${QUOTA_MB}MB)"
fi

# ---------------------------------------------------------------------------
# Install daily scheduler
# ---------------------------------------------------------------------------
info "Setting up daily scheduler (hour: $SCHEDULE_HOUR)..."

BACKUP_SCRIPT="$SCRIPT_DIR/backup.sh"

case "$AGENT_OS" in
    Darwin)
        # macOS: LaunchAgent plist
        PLIST_DIR="$HOME/Library/LaunchAgents"
        PLIST_FILE="$PLIST_DIR/$LABEL.plist"
        mkdir -p "$PLIST_DIR"

        cat > "$PLIST_FILE" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>$BACKUP_SCRIPT</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>$SCHEDULE_HOUR</integer>
        <key>Minute</key>
        <integer>0</integer>
    </dict>
    <key>StandardOutPath</key>
    <string>$STATE_DIR/backup.log</string>
    <key>StandardErrorPath</key>
    <string>$STATE_DIR/backup-error.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>OPENCLAW_DIR</key>
        <string>$OPENCLAW_DIR</string>
        <key>OPENCLAW_BACKUP_URL</key>
        <string>$BACKUP_SERVICE_URL</string>
        <key>PATH</key>
        <string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
PLIST

        # Unload if already loaded, then load
        launchctl unload "$PLIST_FILE" 2>/dev/null || true
        launchctl load "$PLIST_FILE"
        ok "Installed macOS LaunchAgent at $PLIST_FILE"
        ;;

    Linux)
        # Linux: try systemd user timer first, fall back to cron
        if has_cmd systemctl && systemctl --user status >/dev/null 2>&1; then
            SYSTEMD_DIR="$HOME/.config/systemd/user"
            mkdir -p "$SYSTEMD_DIR"

            cat > "$SYSTEMD_DIR/$LABEL.service" <<UNIT
[Unit]
Description=OpenClaw backup

[Service]
Type=oneshot
ExecStart=/bin/bash $BACKUP_SCRIPT
Environment=OPENCLAW_DIR=$OPENCLAW_DIR
Environment=OPENCLAW_BACKUP_URL=$BACKUP_SERVICE_URL
UNIT

            cat > "$SYSTEMD_DIR/$LABEL.timer" <<TIMER
[Unit]
Description=Daily OpenClaw backup

[Timer]
OnCalendar=*-*-* ${SCHEDULE_HOUR}:00:00
Persistent=true
RandomizedDelaySec=900

[Install]
WantedBy=timers.target
TIMER

            systemctl --user daemon-reload
            systemctl --user enable --now "$LABEL.timer"
            ok "Installed systemd timer at $SYSTEMD_DIR/$LABEL.timer"
        else
            # Cron fallback
            CRON_LINE="0 $SCHEDULE_HOUR * * * OPENCLAW_DIR=$OPENCLAW_DIR OPENCLAW_BACKUP_URL=$BACKUP_SERVICE_URL /bin/bash $BACKUP_SCRIPT >> $STATE_DIR/backup.log 2>&1"

            # Add to crontab if not already present
            (crontab -l 2>/dev/null | grep -v "$LABEL" ; echo "# $LABEL"; echo "$CRON_LINE") | crontab -
            ok "Installed cron job"
        fi
        ;;

    *)
        err "Unsupported OS: $AGENT_OS — skipping scheduler setup"
        err "Run backup manually: bash $BACKUP_SCRIPT"
        ;;
esac

# ---------------------------------------------------------------------------
# Run first backup
# ---------------------------------------------------------------------------
info "Running first backup..."
bash "$BACKUP_SCRIPT"

echo ""
ok "Setup complete!"
ok "  Master key: $STATE_DIR/master.key"
ok "  Agent ID:   $(cat "$STATE_DIR/agent.id" 2>/dev/null || echo 'see state dir')"
ok "  Schedule:   daily at ${SCHEDULE_HOUR}:00"
ok "  Service:    $BACKUP_SERVICE_URL"
