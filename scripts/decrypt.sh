#!/usr/bin/env bash
#
# Decrypt a backup file using a master key.
#
# Supports both age and openssl encryption methods. Auto-detects based on key
# file contents (hex string = openssl, AGE-SECRET-KEY = age).
#
# Usage:
#   bash decrypt.sh <backup.tar.gz.enc> <master.key>
#   bash decrypt.sh <backup.tar.gz.enc> <master.key> --manifest manifest.json
#   bash decrypt.sh <backup.tar.gz.enc> <master.key> --iv <hex-iv>
#   bash decrypt.sh <backup.tar.gz.enc> <master.key> -o <output.tar.gz>
#
set -euo pipefail

err()  { printf '\033[1;31m[decrypt]\033[0m %s\n' "$*" >&2; }
ok()   { printf '\033[1;32m[decrypt]\033[0m %s\n' "$*"; }
info() { printf '\033[1;34m[decrypt]\033[0m %s\n' "$*"; }
die()  { err "$@"; exit 1; }

usage() {
    cat <<EOF
Usage: bash decrypt.sh <encrypted-file> <key-file> [options]

Arguments:
  encrypted-file   Path to the .enc file
  key-file         Path to the master key (age secret key or openssl hex key)

Options:
  -o output-file       Output path (default: strips .enc suffix)
  --manifest file      Read IV from manifest.json (for openssl backups)
  --iv hex-string      Provide IV directly (for openssl backups)

For age-encrypted backups:
  bash decrypt.sh backup.tar.gz.enc master.key

For openssl-encrypted backups (need IV from manifest):
  bash decrypt.sh backup.tar.gz.enc master.key --manifest manifest.json
  bash decrypt.sh backup.tar.gz.enc master.key --iv 1a2b3c4d5e6f...
EOF
    exit 1
}

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------
[[ $# -ge 2 ]] || usage

ENC_FILE="$1"
KEY_FILE="$2"
shift 2

OUTPUT=""
MANIFEST=""
IV=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o)         OUTPUT="$2"; shift 2 ;;
        --manifest) MANIFEST="$2"; shift 2 ;;
        --iv)       IV="$2"; shift 2 ;;
        *)          die "Unknown option: $1" ;;
    esac
done

# Default output: strip .enc suffix
if [[ -z "$OUTPUT" ]]; then
    if [[ "$ENC_FILE" == *.enc ]]; then
        OUTPUT="${ENC_FILE%.enc}"
    else
        OUTPUT="${ENC_FILE}.dec"
    fi
fi

# ---------------------------------------------------------------------------
# Validate inputs
# ---------------------------------------------------------------------------
[[ -f "$ENC_FILE" ]] || die "Encrypted file not found: $ENC_FILE"
[[ -f "$KEY_FILE" ]] || die "Key file not found: $KEY_FILE"

# ---------------------------------------------------------------------------
# Detect encryption method from key file contents
# ---------------------------------------------------------------------------
KEY_CONTENT="$(cat "$KEY_FILE")"

if echo "$KEY_CONTENT" | grep -q "AGE-SECRET-KEY-"; then
    ENCRYPT_TOOL="age"
elif echo "$KEY_CONTENT" | grep -qE '^[0-9a-f]{64}$'; then
    ENCRYPT_TOOL="openssl"
else
    die "Unrecognized key format. Expected age secret key (AGE-SECRET-KEY-...) or 64-char hex string."
fi

info "Detected encryption: $ENCRYPT_TOOL"

# ---------------------------------------------------------------------------
# Decrypt
# ---------------------------------------------------------------------------
if [[ "$ENCRYPT_TOOL" == "age" ]]; then
    # Add local bin to PATH (age may be installed here by setup.sh)
    OPENCLAW_DIR="${OPENCLAW_DIR:-$HOME/.openclaw}"
    LOCAL_BIN="$OPENCLAW_DIR/skills/backup/.local/bin"
    [[ -d "$LOCAL_BIN" ]] && export PATH="$LOCAL_BIN:$PATH"

    command -v age &>/dev/null || die "age not found. Install it or run setup.sh first."

    age -d -i "$KEY_FILE" -o "$OUTPUT" "$ENC_FILE" \
        || die "Decryption failed. Is this the correct master key?"
else
    # openssl AES-256-GCM — need IV
    if [[ -n "$MANIFEST" ]]; then
        [[ -f "$MANIFEST" ]] || die "Manifest not found: $MANIFEST"
        IV=$(jq -r '.iv // empty' "$MANIFEST")
        [[ -n "$IV" ]] || die "No IV found in manifest. Was this encrypted with age instead?"
    fi

    [[ -n "$IV" ]] || die "openssl decryption requires an IV. Provide --manifest <manifest.json> or --iv <hex>."

    openssl enc -aes-256-gcm -d \
        -in "$ENC_FILE" \
        -out "$OUTPUT" \
        -K "$KEY_CONTENT" \
        -iv "$IV" \
        2>/dev/null \
        || die "Decryption failed. Is this the correct master key and IV?"
fi

ok "Decrypted: $OUTPUT"
ok "Size: $(wc -c < "$OUTPUT" | tr -d ' ') bytes"

# Hint about extracting if it looks like a tarball
if [[ "$OUTPUT" == *.tar.gz ]]; then
    ok "To extract: tar xzf $OUTPUT -C <destination>"
fi
