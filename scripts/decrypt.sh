#!/usr/bin/env bash
#
# Decrypt an age-encrypted backup file using a master key.
#
# Usage:
#   bash decrypt.sh <backup.tar.gz.enc> <master.key>
#   bash decrypt.sh <backup.tar.gz.enc> <master.key> -o <output.tar.gz>
#
# Examples:
#   bash decrypt.sh backup.tar.gz.enc ~/.openclaw/skills/backup/.state/master.key
#   bash decrypt.sh backup.tar.gz.enc master.key -o restored.tar.gz
#
set -euo pipefail

err()  { printf '\033[1;31m[decrypt]\033[0m %s\n' "$*" >&2; }
ok()   { printf '\033[1;32m[decrypt]\033[0m %s\n' "$*"; }
die()  { err "$@"; exit 1; }

usage() {
    cat <<EOF
Usage: bash decrypt.sh <encrypted-file> <key-file> [-o output-file]

Arguments:
  encrypted-file   Path to the .enc file (age-encrypted backup)
  key-file         Path to the age master key (secret key file)

Options:
  -o output-file   Output path (default: strips .enc suffix, or appends .dec)

Examples:
  bash decrypt.sh backup.tar.gz.enc master.key
  bash decrypt.sh backup.tar.gz.enc master.key -o restored.tar.gz
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
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o) OUTPUT="$2"; shift 2 ;;
        *)  die "Unknown option: $1" ;;
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

# Add local bin to PATH (age may be installed here by setup.sh)
OPENCLAW_DIR="${OPENCLAW_DIR:-$HOME/.openclaw}"
LOCAL_BIN="$OPENCLAW_DIR/skills/backup/.local/bin"
[[ -d "$LOCAL_BIN" ]] && export PATH="$LOCAL_BIN:$PATH"

if ! command -v age &>/dev/null; then
    die "age not found. Install it or run setup.sh first."
fi

# ---------------------------------------------------------------------------
# Decrypt
# ---------------------------------------------------------------------------
age -d -i "$KEY_FILE" -o "$OUTPUT" "$ENC_FILE" \
    || die "Decryption failed. Is this the correct master key?"

ok "Decrypted: $OUTPUT"
ok "Size: $(wc -c < "$OUTPUT" | tr -d ' ') bytes"

# Hint about extracting if it looks like a tarball
if [[ "$OUTPUT" == *.tar.gz ]]; then
    ok "To extract: tar xzf $OUTPUT -C <destination>"
fi
