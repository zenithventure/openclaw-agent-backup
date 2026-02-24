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
          "OPENCLAW_BACKUP_URL": "https://6j95borao8.execute-api.us-east-1.amazonaws.com"
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
| `OPENCLAW_BACKUP_URL` | Backup service endpoint | `https://6j95borao8.execute-api.us-east-1.amazonaws.com` |
| `schedule_hour` | Hour (local time) for daily backup | `3` |
| `exclude_extra` | Additional glob patterns to exclude | `[]` |
| `max_backup_size_mb` | Safety limit on uncompressed size | `500` |

Registration is open — no API key needed. New agents start in **pending** status and require admin approval before backups can run. The scheduler handles this gracefully and retries until the agent is approved.

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
| `POST` | `/v1/agents/register` | None (rate-limited) | Register a new agent (starts as pending) |
| `GET` | `/v1/agents/me` | Bearer | Get agent info and status |
| `POST` | `/v1/agents/me/rotate-token` | Bearer | Rotate API token |
| `POST` | `/v1/backups/upload-url` | Bearer (active) | Get presigned S3 upload URLs |
| `GET` | `/v1/backups` | Bearer | List backup snapshots |
| `GET` | `/v1/backups/{timestamp}` | Bearer | Get backup metadata |
| `POST` | `/v1/backups/download-url` | Bearer | Get presigned S3 download URLs |
| `DELETE` | `/v1/backups/{timestamp}` | Bearer (active) | Soft-delete a backup (recoverable) |
| `DELETE` | `/v1/backups` | Bearer (active) | Soft-delete all backups (recoverable) |
| `POST` | `/v1/backups/{timestamp}/undelete` | Bearer (active) | Restore a soft-deleted backup |
| `GET` | `/v1/admin/agents` | X-API-Key | List agents (optional `?status=` filter) |
| `POST` | `/v1/admin/agents/{id}/approve` | X-API-Key | Approve a pending agent |
| `POST` | `/v1/admin/agents/{id}/suspend` | X-API-Key | Suspend an active agent |

**Agent lifecycle:** `register → pending → (admin approves) → active → (admin suspends) → suspended`

## Server Configuration

| Env Variable | Description | Default |
|-------------|-------------|---------|
| `ADMIN_API_KEY` | API key(s) for admin endpoints, comma-separated for zero-downtime rotation (empty = disabled) | `""` |
| `MAX_UPLOAD_BYTES` | Max single upload size in bytes | `5242880` (5 MB) |
| `MIN_BACKUP_INTERVAL_HOURS` | Minimum hours between backups per agent | `12` |
| `MAX_BACKUPS_PER_AGENT` | Max backups retained per agent (oldest auto-rotated) | `7` |
| `MAX_PENDING_AGENTS` | Global cap on pending registrations | `100` |
| `DELETE_GRACE_HOURS` | Hours before soft-deleted backups are permanently purged | `72` |
| `DEFAULT_QUOTA_BYTES` | Storage quota per agent | `524288000` (500 MB) |
| `REGISTER_RATE_LIMIT` | Registration requests per minute per IP | `10` |
| `RETENTION_DAYS` | DynamoDB TTL retention for backups | `7` |

### Security features

- **Quota enforcement**: Presigned S3 upload URLs include `Content-Length` — S3 rejects uploads that don't match the declared size
- **Upload size limit**: Individual uploads capped at `MAX_UPLOAD_BYTES` (default 5 MB)
- **Backup frequency limit**: Agents can only upload once per `MIN_BACKUP_INTERVAL_HOURS` (default 12h)
- **Backup rotation**: Only `MAX_BACKUPS_PER_AGENT` (default 7) backups are kept; oldest are auto-deleted when a new one arrives
- **Registration throttle**: No more than `MAX_PENDING_AGENTS` (default 100) pending registrations allowed globally
- **Soft-delete protection**: Deleted backups are recoverable via `/undelete` for `DELETE_GRACE_HOURS` (default 72h) — S3 objects are preserved during the grace period
- **Admin key rotation**: `ADMIN_API_KEY` accepts comma-separated keys — deploy with `old,new`, migrate clients, then remove the old key

## License

MIT
