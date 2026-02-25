#!/usr/bin/env bash
#
# OpenClaw Backup Skill — Restore
#
# Downloads, verifies, and decrypts a backup snapshot.
#
# Usage:
#   bash restore.sh                      # restore latest
#   bash restore.sh --date 2026-02-20    # restore specific date
#   bash restore.sh --dry-run            # show what would be restored
#   bash restore.sh --target /path/to    # restore to custom directory
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths & defaults
# ---------------------------------------------------------------------------
OPENCLAW_DIR="${OPENCLAW_DIR:-$HOME/.openclaw}"
STATE_DIR="$OPENCLAW_DIR/skills/backup/.state"
BACKUP_SERVICE_URL="${OPENCLAW_BACKUP_URL:-https://agentbackup.zenithstudio.app}"
TARGET_DIR=""
RESTORE_DATE=""
DRY_RUN=0
TMP_DIR=""

# Add local bin to PATH (age may be installed here by setup.sh)
LOCAL_BIN="$OPENCLAW_DIR/skills/backup/.local/bin"
[[ -d "$LOCAL_BIN" ]] && export PATH="$LOCAL_BIN:$PATH"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { printf '\033[1;34m[restore]\033[0m %s\n' "$*"; }
ok()    { printf '\033[1;32m[restore]\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m[restore]\033[0m %s\n' "$*"; }
err()   { printf '\033[1;31m[restore]\033[0m %s\n' "$*" >&2; }
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
# Parse arguments
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --date)     RESTORE_DATE="$2"; shift 2 ;;
        --target)   TARGET_DIR="$2"; shift 2 ;;
        --dry-run)  DRY_RUN=1; shift ;;
        *)          die "Unknown option: $1" ;;
    esac
done

# Default target: parent of .openclaw (restore overwrites in place)
TARGET_DIR="${TARGET_DIR:-$(dirname "$OPENCLAW_DIR")}"

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------
[[ -f "$STATE_DIR/master.key" ]]  || die "Master key not found. Cannot decrypt without it."
[[ -f "$STATE_DIR/agent.token" ]] || die "Agent token not found. Run setup.sh first."

# encrypt-tool is read from manifest per-backup (MANIFEST_ENCRYPT_TOOL below)

# ---------------------------------------------------------------------------
# Step 1: Resolve backup timestamp
# ---------------------------------------------------------------------------
if [[ -z "$RESTORE_DATE" ]]; then
    info "Finding latest backup..."
    RESPONSE=$(curl -sf -H "$(auth_header)" "$BACKUP_SERVICE_URL/v1/backups?limit=1") \
        || die "Failed to list backups"

    RESTORE_DATE=$(echo "$RESPONSE" | jq -r '.backups[0].timestamp // empty')
    [[ -n "$RESTORE_DATE" ]] || die "No backups found"
    info "Latest backup: $RESTORE_DATE"
else
    info "Restoring backup from: $RESTORE_DATE"
fi

# ---------------------------------------------------------------------------
# Step 2: Request presigned download URLs
# ---------------------------------------------------------------------------
info "Requesting download URLs..."

DOWNLOAD_RESPONSE=$(curl -sf \
    -X POST \
    -H "$(auth_header)" \
    -H "Content-Type: application/json" \
    -d "{\"timestamp\": \"$RESTORE_DATE\"}" \
    "$BACKUP_SERVICE_URL/v1/backups/download-url") \
    || die "Failed to get download URLs"

BACKUP_URL=$(echo "$DOWNLOAD_RESPONSE" | jq -r '.urls["backup.tar.gz.enc"] // empty')
MANIFEST_URL=$(echo "$DOWNLOAD_RESPONSE" | jq -r '.urls["manifest.json"] // empty')

[[ -n "$BACKUP_URL" ]]   || die "No download URL for backup"
[[ -n "$MANIFEST_URL" ]] || die "No download URL for manifest"

# ---------------------------------------------------------------------------
# Step 3: Download manifest first for verification metadata
# ---------------------------------------------------------------------------
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/openclaw-restore-XXXXXX")"

info "Downloading manifest..."
curl -sf -o "$TMP_DIR/manifest.json" "$MANIFEST_URL" \
    || die "Failed to download manifest"

EXPECTED_SHA256=$(jq -r '.files.backup // empty' "$TMP_DIR/manifest.json")
EXPECTED_SIZE=$(jq -r '.encrypted_bytes // empty' "$TMP_DIR/manifest.json")
SOURCE_FILES=$(jq -r '.source_file_count // "unknown"' "$TMP_DIR/manifest.json")
MANIFEST_IV=$(jq -r '.iv // empty' "$TMP_DIR/manifest.json")
MANIFEST_ENCRYPT_TOOL=$(jq -r '.encrypt_tool // "age"' "$TMP_DIR/manifest.json")

info "Backup details: ${SOURCE_FILES} files, ${EXPECTED_SIZE} bytes encrypted"

# ---------------------------------------------------------------------------
# Dry run: stop here and report
# ---------------------------------------------------------------------------
if [[ $DRY_RUN -eq 1 ]]; then
    echo ""
    echo "=== DRY RUN ==="
    echo "Backup date:       $RESTORE_DATE"
    echo "Source files:       $SOURCE_FILES"
    echo "Encrypted size:    $EXPECTED_SIZE bytes"
    echo "SHA-256:           $EXPECTED_SHA256"
    echo "Encryption tool:   $MANIFEST_ENCRYPT_TOOL"
    echo "Restore target:    $TARGET_DIR"
    echo ""
    echo "This would overwrite contents of: $TARGET_DIR/.openclaw/"
    echo "Run without --dry-run to proceed."
    exit 0
fi

# ---------------------------------------------------------------------------
# Step 4: Download encrypted backup
# ---------------------------------------------------------------------------
info "Downloading encrypted backup..."
curl -sf -o "$TMP_DIR/backup.tar.gz.enc" "$BACKUP_URL" \
    || die "Failed to download backup"

# ---------------------------------------------------------------------------
# Step 5: Verify integrity
# ---------------------------------------------------------------------------
info "Verifying integrity..."

ACTUAL_SHA256=$(shasum -a 256 "$TMP_DIR/backup.tar.gz.enc" | cut -d' ' -f1)
ACTUAL_SIZE=$(wc -c < "$TMP_DIR/backup.tar.gz.enc" | tr -d ' ')

if [[ -n "$EXPECTED_SHA256" && "$ACTUAL_SHA256" != "$EXPECTED_SHA256" ]]; then
    die "SHA-256 mismatch! Expected=$EXPECTED_SHA256 Actual=$ACTUAL_SHA256. Backup may be corrupted."
fi

if [[ -n "$EXPECTED_SIZE" && "$ACTUAL_SIZE" != "$EXPECTED_SIZE" ]]; then
    warn "Size mismatch: expected=$EXPECTED_SIZE actual=$ACTUAL_SIZE"
fi

ok "Integrity verified"

# ---------------------------------------------------------------------------
# Step 6: Decrypt
# ---------------------------------------------------------------------------
info "Decrypting..."

DECRYPTED="$TMP_DIR/backup.tar.gz"

if [[ "$MANIFEST_ENCRYPT_TOOL" == "age" ]]; then
    age -d -i "$STATE_DIR/master.key" -o "$DECRYPTED" "$TMP_DIR/backup.tar.gz.enc" \
        || die "Decryption failed. Is this the correct master key?"
else
    # openssl AES-256-GCM
    MASTER_KEY="$(cat "$STATE_DIR/master.key")"
    [[ -n "$MANIFEST_IV" ]] || die "IV not found in manifest (required for openssl decryption)"

    openssl enc -aes-256-gcm -d \
        -in "$TMP_DIR/backup.tar.gz.enc" \
        -out "$DECRYPTED" \
        -K "$MASTER_KEY" \
        -iv "$MANIFEST_IV" \
        2>/dev/null \
        || die "Decryption failed. Is this the correct master key?"
fi

# Remove encrypted blob immediately
rm -f "$TMP_DIR/backup.tar.gz.enc"

ok "Decryption successful"

# ---------------------------------------------------------------------------
# Step 7: Back up current state before overwriting
# ---------------------------------------------------------------------------
if [[ -d "$OPENCLAW_DIR" ]]; then
    PRE_RESTORE_BACKUP="$TARGET_DIR/.openclaw-pre-restore-$(date +%Y%m%d%H%M%S)"
    warn "Backing up current state to $PRE_RESTORE_BACKUP"
    cp -a "$OPENCLAW_DIR" "$PRE_RESTORE_BACKUP"
fi

# ---------------------------------------------------------------------------
# Step 8: Extract
# ---------------------------------------------------------------------------
info "Extracting to $TARGET_DIR ..."

tar xzf "$DECRYPTED" -C "$TARGET_DIR"

ok "Restore complete!"
ok "  Source:  $RESTORE_DATE"
ok "  Target:  $TARGET_DIR/.openclaw/"
ok "  Files:   $SOURCE_FILES"

if [[ -n "${PRE_RESTORE_BACKUP:-}" ]]; then
    ok "  Pre-restore backup: $PRE_RESTORE_BACKUP"
fi

echo ""
warn "The backup skill state (keys, tokens) was preserved from the current install."
warn "If the restored config differs, you may need to re-run setup.sh."
