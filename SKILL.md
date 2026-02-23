---
name: backup
description: Automated encrypted daily backup of agent state. Use when asked to back up, restore, or check backup status. Also triggers during heartbeat if last backup is stale.
version: 1.0.0
homepage: https://github.com/openclaw/openclaw-backup-skill
metadata:
  openclaw:
    emoji: "\U0001F512"
    os:
      - darwin
      - linux
    requires:
      anyBins:
        - age
        - openssl
      bins:
        - curl
        - tar
        - jq
    install:
      - id: age-brew
        kind: brew
        formula: age
        bins:
          - age
        label: Install age encryption (brew)
      - id: jq-brew
        kind: brew
        formula: jq
        bins:
          - jq
        label: Install jq (brew)
---

# Backup

Encrypted daily backup of the entire OpenClaw agent state to a remote backup service.
Backups are encrypted client-side using `age` (X25519 + ChaCha20-Poly1305) before
upload. The backup service never sees plaintext data or keys.

## Setup

Run setup once before first use. This generates encryption keys, registers with the
backup service, and installs a daily scheduler:

```bash
bash {baseDir}/scripts/setup.sh
```

Setup is fully automatic and requires no human input. It will:
1. Generate an age keypair (master encryption key)
2. Register this agent with the backup service
3. Install a daily scheduler (launchd on macOS, systemd timer on Linux, cron fallback)
4. Run the first backup immediately

## Commands

### Immediate backup

```bash
bash {baseDir}/scripts/backup.sh
```

### Check backup status

```bash
bash {baseDir}/scripts/backup.sh --status
```

Shows: last backup time, size, number of available snapshots, and service quota.

### List available backups

```bash
bash {baseDir}/scripts/backup.sh --list
```

### Restore from backup

```bash
bash {baseDir}/scripts/restore.sh                    # restore latest
bash {baseDir}/scripts/restore.sh --date 2026-02-20  # restore specific date
bash {baseDir}/scripts/restore.sh --dry-run           # show what would be restored
```

### Uninstall

```bash
bash {baseDir}/scripts/uninstall.sh
```

Removes the daily scheduler. Does NOT delete the master key or remote backups.

## Heartbeat behavior

During heartbeat, check if backup is stale. If the file
`~/.openclaw/skills/backup/.state/last-backup` is missing or its timestamp
is older than 24 hours, run an immediate backup:

```bash
bash {baseDir}/scripts/backup.sh
```

## Configuration

Set via `openclaw.json` under `skills.entries.backup`:

```json
{
  "skills": {
    "entries": {
      "backup": {
        "env": {
          "OPENCLAW_BACKUP_URL": "https://backup.openclaw.ai"
        },
        "config": {
          "schedule_hour": 3,
          "exclude_extra": [],
          "max_backup_size_mb": 500
        }
      }
    }
  }
}
```

- `OPENCLAW_BACKUP_URL` -- Backup service endpoint (default: `https://backup.openclaw.ai`)
- `schedule_hour` -- Hour of day for scheduled backup in local time (default: 3)
- `exclude_extra` -- Additional glob patterns to exclude from backup
- `max_backup_size_mb` -- Safety limit on uncompressed backup size (default: 500)

## What gets backed up

Everything under `~/.openclaw/` except:

- `skills/backup/.state/` (encryption keys and tokens must not go to the same service)
- `*.lock`, `*.tmp`, `*.pid`
- `.git/`, `node_modules/`, `__pycache__/`, `.venv/`
- Files larger than 50 MB

## Security

- Encryption happens locally before any data leaves the machine
- The backup service stores opaque encrypted blobs only
- Master key is stored at `~/.openclaw/skills/backup/.state/master.key` with 0600 permissions
- Each backup uses a fresh ephemeral key internally (age protocol)
- Transport is HTTPS with TLS 1.2+
- Agent bearer tokens are scoped and rotatable

## Guardrails

- Never log or display the master key contents
- Never transmit unencrypted backup data
- Never store the master key in the same location as the backups
- If master key is lost, backups are unrecoverable -- remind the user to store the recovery key
