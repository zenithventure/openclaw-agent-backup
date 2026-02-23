# OpenClaw Backup Skill

Encrypted daily backup of OpenClaw agent state to a remote service. Backups are encrypted client-side using `age` (X25519 + ChaCha20-Poly1305) before upload — the server never sees plaintext data or keys.

## Install

Install this skill into your OpenClaw agent:

```bash
openclaw skill install https://github.com/zenithventure/openclaw-agent-backup
```

Or clone manually into the skills directory:

```bash
git clone https://github.com/zenithventure/openclaw-agent-backup.git \
  ~/.openclaw/skills/backup
```

Then run setup (fully automatic, no human input required):

```bash
bash ~/.openclaw/skills/backup/scripts/setup.sh
```

Setup will:
1. Generate an `age` keypair (master encryption key)
2. Register this agent with the backup service
3. Install a daily scheduler (launchd on macOS, systemd/cron on Linux)
4. Run the first backup immediately

## Requirements

- **OS**: macOS or Linux
- **Encryption**: `age` (recommended) or `openssl`
- **Tools**: `curl`, `tar`, `jq`

Missing dependencies are listed in the skill manifest and can be auto-installed:

```bash
brew install age jq   # macOS
```

## Usage

```bash
# Run a backup now
bash ~/.openclaw/skills/backup/scripts/backup.sh

# Check backup status
bash ~/.openclaw/skills/backup/scripts/backup.sh --status

# List available snapshots
bash ~/.openclaw/skills/backup/scripts/backup.sh --list

# Restore latest backup
bash ~/.openclaw/skills/backup/scripts/restore.sh

# Restore a specific date
bash ~/.openclaw/skills/backup/scripts/restore.sh --date 2026-02-20

# Preview what would be restored
bash ~/.openclaw/skills/backup/scripts/restore.sh --dry-run

# Uninstall scheduler (keeps keys and remote backups)
bash ~/.openclaw/skills/backup/scripts/uninstall.sh
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

| Variable | Description | Default |
|----------|-------------|---------|
| `OPENCLAW_BACKUP_URL` | Backup service endpoint | `https://backup.openclaw.ai` |
| `schedule_hour` | Hour (local time) for daily backup | `3` |
| `exclude_extra` | Additional glob patterns to exclude | `[]` |
| `max_backup_size_mb` | Safety limit on uncompressed size | `500` |

## What gets backed up

Everything under `~/.openclaw/` except:

- `skills/backup/.state/` (encryption keys and tokens)
- `*.lock`, `*.tmp`, `*.pid`
- `.git/`, `node_modules/`, `__pycache__/`, `.venv/`
- Files larger than 50 MB

## Security

- Encryption happens locally before any data leaves the machine
- The backup service stores opaque encrypted blobs only
- Master key is stored at `~/.openclaw/skills/backup/.state/master.key` (mode `0600`)
- Each backup uses a fresh ephemeral key internally (age protocol)
- Transport is HTTPS with TLS 1.2+
- Agent bearer tokens are scoped and rotatable
- If master key is lost, backups are **unrecoverable**

## Self-hosting the backup service

The `service/` directory contains the Go backend, deployable as an AWS Lambda (SAM) or as a standalone HTTP server.

### AWS (SAM)

```bash
cd service
sam build
sam deploy --guided
```

Resources created: Lambda function, API Gateway v2, DynamoDB tables, S3 bucket with encryption and lifecycle expiry.

### Local development

```bash
docker compose up
```

This starts the API on `http://localhost:8080` with MinIO for S3-compatible storage and SQLite for metadata. Point your agent at it:

```bash
OPENCLAW_BACKUP_URL=http://localhost:8080 bash scripts/setup.sh
```

## API

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/healthz` | No | Health check |
| `POST` | `/v1/agents/register` | No (rate-limited) | Register a new agent |
| `GET` | `/v1/agents/me` | Bearer | Get agent info and quota |
| `POST` | `/v1/agents/me/rotate-token` | Bearer | Rotate API token |
| `POST` | `/v1/backups/upload-url` | Bearer | Get presigned S3 upload URLs |
| `GET` | `/v1/backups` | Bearer | List backup snapshots |
| `GET` | `/v1/backups/{timestamp}` | Bearer | Get backup metadata |
| `POST` | `/v1/backups/download-url` | Bearer | Get presigned S3 download URLs |
| `DELETE` | `/v1/backups/{timestamp}` | Bearer | Delete a specific backup |
| `DELETE` | `/v1/backups` | Bearer | Delete all backups |

## License

MIT
