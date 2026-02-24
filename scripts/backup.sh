#!/usr/bin/env bash
#
# OpenClaw Backup Skill — Backup
#
# Compresses ~/.openclaw, encrypts client-side, and uploads to the backup service.
#
# Usage:
#   bash backup.sh              # run backup
#   bash backup.sh --status     # show backup status
#   bash backup.sh --list       # list available snapshots
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths & defaults
# ---------------------------------------------------------------------------
OPENCLAW_DIR="${OPENCLAW_DIR:-$HOME/.openclaw}"
STATE_DIR="$OPENCLAW_DIR/skills/backup/.state"
BACKUP_SERVICE_URL="${OPENCLAW_BACKUP_URL:-https://6j95borao8.execute-api.us-east-1.amazonaws.com}"

# Add local bin to PATH (age may be installed here by setup.sh)
LOCAL_BIN="$OPENCLAW_DIR/skills/backup/.local/bin"
[[ -d "$LOCAL_BIN" ]] && export PATH="$LOCAL_BIN:$PATH"
MAX_SIZE_MB="${OPENCLAW_BACKUP_MAX_MB:-500}"
TIMESTAMP="$(date -u +%Y-%m-%dT%H%M%SZ)"
TMP_DIR=""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { printf '\033[1;34m[backup]\033[0m %s\n' "$*"; }
ok()    { printf '\033[1;32m[backup]\033[0m %s\n' "$*"; }
err()   { printf '\033[1;31m[backup]\033[0m %s\n' "$*" >&2; }
die()   { err "$@"; exit 1; }

cleanup() {
    if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
        rm -rf "$TMP_DIR"
    fi
}
trap cleanup EXIT

auth_header() {
    local token
    token="$(cat "$STATE_DIR/agent.token" 2>/dev/null)" || die "Agent token not found. Run setup.sh first."
    echo "Authorization: Bearer $token"
}

# ---------------------------------------------------------------------------
# Subcommands: --status, --list
# ---------------------------------------------------------------------------
if [[ "${1:-}" == "--status" ]]; then
    info "Checking backup status..."

    # Check live agent status from the service
    AGENT_RESP=$(curl -sf -H "$(auth_header)" "$BACKUP_SERVICE_URL/v1/agents/me" 2>/dev/null) || true
    if [[ -n "$AGENT_RESP" ]]; then
        LIVE_STATUS=$(echo "$AGENT_RESP" | jq -r '.status // "unknown"')
        echo "$LIVE_STATUS" > "$STATE_DIR/agent.status"
        echo "Agent status: $LIVE_STATUS"
    else
        echo "Agent status: $(cat "$STATE_DIR/agent.status" 2>/dev/null || echo 'unknown')"
    fi

    if [[ -f "$STATE_DIR/last-backup" ]]; then
        LAST="$(cat "$STATE_DIR/last-backup")"
        echo "Last backup: $LAST"
    else
        echo "Last backup: never"
    fi

    if [[ -f "$STATE_DIR/last-manifest.json" ]]; then
        SIZE=$(jq -r '.encrypted_bytes // "unknown"' "$STATE_DIR/last-manifest.json")
        FILES=$(jq -r '.source_file_count // "unknown"' "$STATE_DIR/last-manifest.json")
        echo "Last backup size: ${SIZE} bytes (${FILES} source files)"
    fi

    # Query service for quota and snapshot count
    RESPONSE=$(curl -sf -H "$(auth_header)" "$BACKUP_SERVICE_URL/v1/backups?count_only=true" 2>/dev/null) || true
    if [[ -n "$RESPONSE" ]]; then
        COUNT=$(echo "$RESPONSE" | jq -r '.count // "unknown"')
        USED=$(echo "$RESPONSE" | jq -r '.used_bytes // "unknown"')
        QUOTA=$(echo "$RESPONSE" | jq -r '.quota_bytes // "unknown"')
        echo "Snapshots: $COUNT"
        echo "Storage used: $USED / $QUOTA bytes"
    fi
    exit 0
fi

if [[ "${1:-}" == "--list" ]]; then
    info "Listing available backups..."
    RESPONSE=$(curl -sf -H "$(auth_header)" "$BACKUP_SERVICE_URL/v1/backups" 2>/dev/null) \
        || die "Failed to list backups. Is the service reachable?"

    echo "$RESPONSE" | jq -r '
        .backups[]? |
        "\(.timestamp)\t\(.encrypted_bytes) bytes\t\(.source_file_count) files"
    ' | column -t -s $'\t'
    exit 0
fi

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------
[[ -d "$OPENCLAW_DIR" ]]        || die "OpenClaw directory not found: $OPENCLAW_DIR"
[[ -f "$STATE_DIR/master.key" ]] || die "Master key not found. Run setup.sh first."
[[ -f "$STATE_DIR/agent.token" ]] || die "Agent token not found. Run setup.sh first."

ENCRYPT_TOOL="$(cat "$STATE_DIR/encrypt-tool" 2>/dev/null || echo 'age')"

# ---------------------------------------------------------------------------
# Check agent status (discovers approval, updates local state)
# ---------------------------------------------------------------------------
AGENT_STATUS_RESP=$(curl -sf -H "$(auth_header)" "$BACKUP_SERVICE_URL/v1/agents/me" 2>/dev/null) || true
if [[ -n "$AGENT_STATUS_RESP" ]]; then
    REMOTE_STATUS=$(echo "$AGENT_STATUS_RESP" | jq -r '.status // empty')
    if [[ -n "$REMOTE_STATUS" ]]; then
        echo "$REMOTE_STATUS" > "$STATE_DIR/agent.status"
        if [[ "$REMOTE_STATUS" != "active" ]]; then
            ok "Agent status: $REMOTE_STATUS. Backups will start once an admin approves this agent."
            exit 0
        fi
    fi
fi

# Check staleness — skip if backup ran less than 20 hours ago (avoids duplicate runs)
if [[ -f "$STATE_DIR/last-backup" ]]; then
    LAST_EPOCH=$(date -j -f "%Y-%m-%dT%H%M%SZ" "$(cat "$STATE_DIR/last-backup")" +%s 2>/dev/null \
                 || date -d "$(cat "$STATE_DIR/last-backup")" +%s 2>/dev/null \
                 || echo 0)
    NOW_EPOCH=$(date +%s)
    HOURS_AGO=$(( (NOW_EPOCH - LAST_EPOCH) / 3600 ))
    if [[ $HOURS_AGO -lt 12 && "${FORCE_BACKUP:-}" != "1" ]]; then
        ok "Last backup was ${HOURS_AGO}h ago (< 12h). Skipping. Set FORCE_BACKUP=1 to override."
        exit 0
    fi
fi

# ---------------------------------------------------------------------------
# Step 1: Create compressed tarball with exclusions
# ---------------------------------------------------------------------------
info "Creating backup snapshot..."

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/openclaw-backup-XXXXXX")"
TARBALL="$TMP_DIR/backup.tar.gz"

# Build exclusion list
EXCLUDE_PATTERNS=(
    --exclude='skills/backup/.state'
    --exclude='*.lock'
    --exclude='*.tmp'
    --exclude='*.pid'
    --exclude='.git'
    --exclude='node_modules'
    --exclude='__pycache__'
    --exclude='.venv'
    --exclude='.DS_Store'
    --exclude='*.log'
)

# Create tarball
tar czf "$TARBALL" \
    "${EXCLUDE_PATTERNS[@]}" \
    -C "$(dirname "$OPENCLAW_DIR")" \
    "$(basename "$OPENCLAW_DIR")" \
    2>/dev/null

TARBALL_SIZE=$(wc -c < "$TARBALL" | tr -d ' ')
TARBALL_SIZE_MB=$((TARBALL_SIZE / 1048576))

if [[ $TARBALL_SIZE_MB -gt $MAX_SIZE_MB ]]; then
    die "Backup too large: ${TARBALL_SIZE_MB}MB exceeds limit of ${MAX_SIZE_MB}MB"
fi

# Count source files for manifest
SOURCE_FILE_COUNT=$(tar tzf "$TARBALL" 2>/dev/null | wc -l | tr -d ' ')

info "Tarball created: ${TARBALL_SIZE_MB}MB (${SOURCE_FILE_COUNT} files)"

# ---------------------------------------------------------------------------
# Step 2: Encrypt the tarball
# ---------------------------------------------------------------------------
info "Encrypting backup..."

ENCRYPTED="$TMP_DIR/backup.tar.gz.enc"

if [[ "$ENCRYPT_TOOL" == "age" ]]; then
    # age: read the recipient (public key) from state
    AGE_RECIPIENT="$(cat "$STATE_DIR/recipient.txt" 2>/dev/null)" \
        || die "age recipient not found in $STATE_DIR/recipient.txt"

    age -r "$AGE_RECIPIENT" -o "$ENCRYPTED" "$TARBALL"
else
    # openssl: AES-256-GCM with the master key
    MASTER_KEY="$(cat "$STATE_DIR/master.key")"

    # Generate random IV (96 bits = 12 bytes for GCM)
    IV=$(openssl rand -hex 12)

    openssl enc -aes-256-gcm \
        -in "$TARBALL" \
        -out "$ENCRYPTED" \
        -K "$MASTER_KEY" \
        -iv "$IV" \
        2>/dev/null

    # Store IV alongside encrypted file (needed for decryption)
    echo "$IV" > "$TMP_DIR/iv.txt"
fi

# Remove plaintext tarball immediately
rm -f "$TARBALL"

ENCRYPTED_SIZE=$(wc -c < "$ENCRYPTED" | tr -d ' ')
info "Encrypted: $(( ENCRYPTED_SIZE / 1048576 ))MB"

# ---------------------------------------------------------------------------
# Step 3: Compute checksums
# ---------------------------------------------------------------------------
ENCRYPTED_SHA256=$(shasum -a 256 "$ENCRYPTED" | cut -d' ' -f1)

# ---------------------------------------------------------------------------
# Step 4: Build manifest
# ---------------------------------------------------------------------------
info "Building manifest..."

MANIFEST="$TMP_DIR/manifest.json"

MANIFEST_CONTENT=$(jq -n \
    --arg version "1" \
    --arg timestamp "$TIMESTAMP" \
    --arg agent_id "$(cat "$STATE_DIR/agent.id" 2>/dev/null || echo '')" \
    --arg encrypt_tool "$ENCRYPT_TOOL" \
    --arg encrypted_sha256 "$ENCRYPTED_SHA256" \
    --argjson encrypted_bytes "$ENCRYPTED_SIZE" \
    --argjson source_file_count "$SOURCE_FILE_COUNT" \
    --argjson source_bytes "$(du -sb "$OPENCLAW_DIR" 2>/dev/null | cut -f1 || echo 0)" \
    --arg iv "$(cat "$TMP_DIR/iv.txt" 2>/dev/null || echo '')" \
    --arg skill_version "1.0.0" \
    '{
        version: ($version | tonumber),
        timestamp: $timestamp,
        agent_id: $agent_id,
        encrypt_tool: $encrypt_tool,
        files: {
            backup: $encrypted_sha256
        },
        encrypted_bytes: $encrypted_bytes,
        source_file_count: $source_file_count,
        source_bytes: ($source_bytes | tonumber),
        iv: (if $iv == "" then null else $iv end),
        skill_version: $skill_version
    }')

echo "$MANIFEST_CONTENT" > "$MANIFEST"

# ---------------------------------------------------------------------------
# Step 5: Request presigned upload URLs
# ---------------------------------------------------------------------------
info "Requesting upload URLs..."

UPLOAD_REQUEST=$(jq -n \
    --arg timestamp "$TIMESTAMP" \
    --argjson encrypted_bytes "$ENCRYPTED_SIZE" \
    --arg encrypted_sha256 "$ENCRYPTED_SHA256" \
    '{
        timestamp: $timestamp,
        files: ["backup.tar.gz.enc", "manifest.json"],
        encrypted_bytes: $encrypted_bytes,
        encrypted_sha256: $encrypted_sha256
    }')

UPLOAD_HTTP_STATUS=$(curl -s -o "$TMP_DIR/upload-response.json" -w "%{http_code}" \
    -X POST \
    -H "$(auth_header)" \
    -H "Content-Type: application/json" \
    -d "$UPLOAD_REQUEST" \
    "$BACKUP_SERVICE_URL/v1/backups/upload-url")

if [[ "$UPLOAD_HTTP_STATUS" == "403" ]]; then
    ok "Agent not yet approved. Backups will start once an admin approves this agent."
    exit 0
fi

if [[ ! "$UPLOAD_HTTP_STATUS" =~ ^2 ]]; then
    die "Failed to get upload URLs from backup service (HTTP $UPLOAD_HTTP_STATUS)"
fi

UPLOAD_RESPONSE=$(cat "$TMP_DIR/upload-response.json")

BACKUP_UPLOAD_URL=$(echo "$UPLOAD_RESPONSE" | jq -r '.urls["backup.tar.gz.enc"] // empty')
MANIFEST_UPLOAD_URL=$(echo "$UPLOAD_RESPONSE" | jq -r '.urls["manifest.json"] // empty')

[[ -n "$BACKUP_UPLOAD_URL" ]]   || die "No upload URL for backup blob"
[[ -n "$MANIFEST_UPLOAD_URL" ]] || die "No upload URL for manifest"

# ---------------------------------------------------------------------------
# Step 6: Upload encrypted backup and manifest
# ---------------------------------------------------------------------------
info "Uploading encrypted backup ($(( ENCRYPTED_SIZE / 1048576 ))MB)..."

# Upload backup blob (Content-Length must match the presigned URL)
HTTP_STATUS=$(curl -sf -o /dev/null -w "%{http_code}" \
    -X PUT \
    -H "Content-Type: application/octet-stream" \
    -H "Content-Length: $ENCRYPTED_SIZE" \
    --data-binary "@$ENCRYPTED" \
    "$BACKUP_UPLOAD_URL") \
    || die "Failed to upload backup blob"

[[ "$HTTP_STATUS" =~ ^2 ]] || die "Upload failed with HTTP $HTTP_STATUS"

info "Uploading manifest..."

# Upload manifest
HTTP_STATUS=$(curl -sf -o /dev/null -w "%{http_code}" \
    -X PUT \
    -H "Content-Type: application/json" \
    --data-binary "@$MANIFEST" \
    "$MANIFEST_UPLOAD_URL") \
    || die "Failed to upload manifest"

[[ "$HTTP_STATUS" =~ ^2 ]] || die "Manifest upload failed with HTTP $HTTP_STATUS"

# ---------------------------------------------------------------------------
# Step 7: Verify upload
# ---------------------------------------------------------------------------
info "Verifying upload..."

VERIFY_RESPONSE=$(curl -sf \
    -H "$(auth_header)" \
    "$BACKUP_SERVICE_URL/v1/backups/$TIMESTAMP" 2>/dev/null) || true

if [[ -n "$VERIFY_RESPONSE" ]]; then
    REMOTE_SHA=$(echo "$VERIFY_RESPONSE" | jq -r '.encrypted_sha256 // empty')
    if [[ "$REMOTE_SHA" == "$ENCRYPTED_SHA256" ]]; then
        ok "Upload verified (SHA-256 match)"
    else
        err "WARNING: SHA-256 mismatch! Local=$ENCRYPTED_SHA256 Remote=$REMOTE_SHA"
    fi
fi

# ---------------------------------------------------------------------------
# Step 8: Update state
# ---------------------------------------------------------------------------
echo "$TIMESTAMP" > "$STATE_DIR/last-backup"
cp "$MANIFEST" "$STATE_DIR/last-manifest.json"

ok "Backup complete: $TIMESTAMP ($(( ENCRYPTED_SIZE / 1048576 ))MB, ${SOURCE_FILE_COUNT} files)"
