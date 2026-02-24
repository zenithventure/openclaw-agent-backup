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
BACKUP_SERVICE_URL="${OPENCLAW_BACKUP_URL:-https://6j95borao8.execute-api.us-east-1.amazonaws.com}"
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
# Local bin directory (for user-space installs)
# ---------------------------------------------------------------------------
LOCAL_BIN="$OPENCLAW_DIR/skills/backup/.local/bin"
if [[ -d "$LOCAL_BIN" ]]; then
    export PATH="$LOCAL_BIN:$PATH"
fi

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------
info "Running preflight checks..."

[[ -d "$OPENCLAW_DIR" ]] || die "OpenClaw directory not found at $OPENCLAW_DIR"

for cmd in curl tar jq; do
    has_cmd "$cmd" || die "Required command not found: $cmd"
done

# ---------------------------------------------------------------------------
# Install age locally if not available
# ---------------------------------------------------------------------------
if ! has_cmd age; then
    info "age not found in PATH, installing locally..."

    AGE_VERSION="1.2.1"
    AGENT_OS="$(uname -s)"
    AGENT_ARCH="$(uname -m)"

    case "${AGENT_OS}_${AGENT_ARCH}" in
        Linux_x86_64)  AGE_ARCHIVE="age-v${AGE_VERSION}-linux-amd64.tar.gz" ;;
        Linux_aarch64) AGE_ARCHIVE="age-v${AGE_VERSION}-linux-arm64.tar.gz" ;;
        Darwin_x86_64) AGE_ARCHIVE="age-v${AGE_VERSION}-darwin-amd64.tar.gz" ;;
        Darwin_arm64)  AGE_ARCHIVE="age-v${AGE_VERSION}-darwin-arm64.tar.gz" ;;
        *) die "Unsupported platform for age install: ${AGENT_OS}_${AGENT_ARCH}. Install age manually or use openssl." ;;
    esac

    AGE_URL="https://github.com/FiloSottile/age/releases/download/v${AGE_VERSION}/${AGE_ARCHIVE}"
    AGE_TMP="$(mktemp -d)"

    info "Downloading age v${AGE_VERSION} from $AGE_URL ..."
    curl -sfL -o "$AGE_TMP/$AGE_ARCHIVE" "$AGE_URL" \
        || die "Failed to download age. Install it manually: https://github.com/FiloSottile/age"

    tar xzf "$AGE_TMP/$AGE_ARCHIVE" -C "$AGE_TMP"

    mkdir -p "$LOCAL_BIN"
    cp "$AGE_TMP/age/age" "$LOCAL_BIN/age"
    cp "$AGE_TMP/age/age-keygen" "$LOCAL_BIN/age-keygen"
    chmod +x "$LOCAL_BIN/age" "$LOCAL_BIN/age-keygen"
    rm -rf "$AGE_TMP"

    export PATH="$LOCAL_BIN:$PATH"

    has_cmd age || die "age install failed — binary not found after install"
    ok "Installed age v${AGE_VERSION} to $LOCAL_BIN"
fi

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
        # age-keygen writes the full key (comments + secret) to stdout,
        # and "Public key: age1..." to stderr.
        age-keygen -o "$STATE_DIR/master.key" 2>"$STATE_DIR/master.key.pub"
        chmod 600 "$STATE_DIR/master.key"
        chmod 600 "$STATE_DIR/master.key.pub"

        # Extract the public key (recipient) from the key file comment
        AGE_RECIPIENT=$(grep -oE 'age1[a-z0-9]+' "$STATE_DIR/master.key")
        [[ -n "$AGE_RECIPIENT" ]] || die "Failed to extract age recipient from master key"
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

    # Emit structured recovery info for agent parsing
    RECOVERY_JSON=$(jq -cn \
        --arg key_path "$STATE_DIR/master.key" \
        --arg public_key "$(cat "$STATE_DIR/recipient.txt" 2>/dev/null || echo '')" \
        --arg encryption_tool "$ENCRYPT_TOOL" \
        '{recovery_key_path: $key_path, public_key: $public_key, encryption_tool: $encryption_tool}')
    echo "[OPENCLAW_RECOVERY_INFO]${RECOVERY_JSON}"
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

    REGISTER_RESPONSE=$(curl -sf -X POST \
        -H "Content-Type: application/json" \
        -d "$REGISTER_PAYLOAD" \
        "$BACKUP_SERVICE_URL/v1/agents/register" 2>&1) \
        || die "Registration failed. Is the backup service reachable at $BACKUP_SERVICE_URL?"

    # Parse response
    AGENT_ID=$(echo "$REGISTER_RESPONSE" | jq -r '.agent_id // empty')
    AGENT_TOKEN=$(echo "$REGISTER_RESPONSE" | jq -r '.token // empty')
    AGENT_STATUS=$(echo "$REGISTER_RESPONSE" | jq -r '.status // "active"')
    QUOTA_MB=$(echo "$REGISTER_RESPONSE" | jq -r '.quota_mb // "500"')

    [[ -n "$AGENT_ID" ]]    || die "Registration response missing agent_id"
    [[ -n "$AGENT_TOKEN" ]] || die "Registration response missing token"

    echo "$AGENT_TOKEN" > "$STATE_DIR/agent.token"
    chmod 600 "$STATE_DIR/agent.token"

    echo "$AGENT_ID" > "$STATE_DIR/agent.id"
    echo "$AGENT_STATUS" > "$STATE_DIR/agent.status"

    ok "Registered as $AGENT_ID (quota: ${QUOTA_MB}MB, status: $AGENT_STATUS)"
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
        <string>$LOCAL_BIN:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
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
Environment=PATH=$LOCAL_BIN:/usr/local/bin:/usr/bin:/bin
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
            CRON_LINE="0 $SCHEDULE_HOUR * * * PATH=$LOCAL_BIN:/usr/local/bin:/usr/bin:/bin OPENCLAW_DIR=$OPENCLAW_DIR OPENCLAW_BACKUP_URL=$BACKUP_SERVICE_URL /bin/bash $BACKUP_SCRIPT >> $STATE_DIR/backup.log 2>&1"

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
# Run first backup (skip if agent is pending approval)
# ---------------------------------------------------------------------------
CURRENT_STATUS="$(cat "$STATE_DIR/agent.status" 2>/dev/null || echo 'active')"

if [[ "$CURRENT_STATUS" == "pending" ]]; then
    echo ""
    info "Agent registered but pending admin approval."
    info "Backups will start automatically once approved."
    info "The scheduler is installed and will retry on schedule."
else
    info "Running first backup..."
    bash "$BACKUP_SCRIPT"
fi

echo ""
ok "Setup complete!"
ok "  Master key: $STATE_DIR/master.key"
ok "  Agent ID:   $(cat "$STATE_DIR/agent.id" 2>/dev/null || echo 'see state dir')"
ok "  Status:     $CURRENT_STATUS"
ok "  Schedule:   daily at ${SCHEDULE_HOUR}:00"
ok "  Service:    $BACKUP_SERVICE_URL"
