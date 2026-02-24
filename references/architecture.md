# Backup Skill — Architecture Reference

## Encryption Model

Envelope encryption with `age` (preferred) or `openssl` fallback.

### With age (X25519 + ChaCha20-Poly1305)

```
Master keypair (age identity)      ← Generated once at setup
  │                                   Stored at .state/master.key (0600)
  │
  │  age encrypts to the public key (recipient)
  │  internally generates ephemeral X25519 key per file
  v
Encrypted backup blob              ← Each backup has unique ephemeral key
```

age handles key wrapping internally — no explicit DEK/KEK management needed.
Each encryption produces a unique ciphertext even for identical input.

### With openssl fallback (AES-256-GCM)

```
Master key (256-bit random)        ← Generated once at setup
  │                                   Stored at .state/master.key (0600)
  │
  │  AES-256-GCM with random 96-bit IV per backup
  v
Encrypted backup blob              ← IV stored in manifest.json
```

## Directory Layout

```
~/.openclaw/skills/backup/
  SKILL.md                          # Skill definition (frontmatter + instructions)
  scripts/
    setup.sh                        # One-time setup
    backup.sh                       # Daily backup
    restore.sh                      # Restore from backup
    uninstall.sh                    # Remove scheduler
  references/
    architecture.md                 # This file
  .state/                           # Created by setup.sh (chmod 700)
    master.key                      # age identity or openssl hex key (chmod 600)
    master.key.pub                  # age public key stderr output (age only)
    recipient.txt                   # age recipient string (age only)
    encrypt-tool                    # "age" or "openssl"
    agent.id                        # Server-assigned agent ID
    agent.token                     # Bearer token for API auth (chmod 600)
    last-backup                     # Timestamp of last successful backup
    last-manifest.json              # Manifest from last backup
    backup.log                      # Scheduler stdout (macOS)
    backup-error.log                # Scheduler stderr (macOS)
```

## Backup Service API

Base URL configured via `OPENCLAW_BACKUP_URL` (default: `https://6j95borao8.execute-api.us-east-1.amazonaws.com`).

### POST /v1/agents/register

Self-enrollment. No auth required (rate-limited).

**Request:**
```json
{
  "agent_name": "my-agent",
  "hostname": "macbook",
  "os": "Darwin",
  "arch": "arm64",
  "openclaw_version": "0.9.0",
  "machine_fingerprint": "sha256:...",
  "encrypt_tool": "age",
  "public_key": "age1..."
}
```

**Response:**
```json
{
  "agent_id": "ag_abc123def456",
  "token": "ocb_...",
  "quota_mb": 500,
  "backup_prefix": "ag_abc123def456/"
}
```

### POST /v1/backups/upload-url

Request presigned S3 PUT URLs. Bearer token required.

**Request:**
```json
{
  "timestamp": "2026-02-22T030000Z",
  "files": ["backup.tar.gz.enc", "manifest.json"],
  "encrypted_bytes": 52429100,
  "encrypted_sha256": "a1b2c3..."
}
```

**Response:**
```json
{
  "urls": {
    "backup.tar.gz.enc": "https://s3.../presigned-put-url",
    "manifest.json": "https://s3.../presigned-put-url"
  },
  "expires_in": 900
}
```

### GET /v1/backups

List backup snapshots. Bearer token required.

Query params: `limit`, `count_only`

**Response:**
```json
{
  "backups": [
    {
      "timestamp": "2026-02-22T030000Z",
      "encrypted_bytes": 52429100,
      "source_file_count": 247,
      "encrypted_sha256": "a1b2c3..."
    }
  ],
  "count": 30,
  "used_bytes": 1572864000,
  "quota_bytes": 524288000
}
```

### POST /v1/backups/download-url

Request presigned S3 GET URLs. Bearer token required.

### GET /v1/backups/{timestamp}

Get metadata for a specific backup (for verification).

### DELETE /v1/backups

Delete all backups (used by uninstall --purge-all).

### DELETE /v1/backups/{timestamp}

Delete a specific backup.

## Scheduler Details

### macOS (launchd)

Plist at `~/Library/LaunchAgents/ai.openclaw.backup.plist`.

- `StartCalendarInterval` fires daily at configured hour
- If machine is asleep at fire time, launchd runs it on next wake
- Logs to `.state/backup.log` and `.state/backup-error.log`

### Linux (systemd timer)

Files at `~/.config/systemd/user/ai.openclaw.backup.{service,timer}`.

- `Persistent=true` fires missed jobs on boot
- `RandomizedDelaySec=900` avoids thundering herd
- Logs via `journalctl --user -u ai.openclaw.backup`

### Linux (cron fallback)

Entry in user crontab. No catch-up for missed runs — the heartbeat
integration compensates (SKILL.md instructs the agent to check staleness).

## Manifest Schema

```json
{
  "version": 1,
  "timestamp": "2026-02-22T030000Z",
  "agent_id": "ag_abc123",
  "encrypt_tool": "age",
  "files": {
    "backup": "sha256_of_encrypted_blob"
  },
  "encrypted_bytes": 52429100,
  "source_file_count": 247,
  "source_bytes": 52400000,
  "iv": null,
  "skill_version": "1.0.0"
}
```

- `iv` is null for age (not needed), present for openssl
- `files.backup` is the SHA-256 of the encrypted blob for integrity verification
- GCM authentication tag provides additional tamper detection on decrypt
