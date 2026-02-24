package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements DataStore using SQLite (for local dev and tests).
type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	if err := migrateSQLite(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func migrateSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agents (
			id               TEXT PRIMARY KEY,
			name             TEXT NOT NULL,
			hostname         TEXT NOT NULL DEFAULT '',
			os               TEXT NOT NULL DEFAULT '',
			arch             TEXT NOT NULL DEFAULT '',
			openclaw_version TEXT NOT NULL DEFAULT '',
			fingerprint      TEXT NOT NULL DEFAULT '',
			encrypt_tool     TEXT NOT NULL DEFAULT 'age',
			public_key       TEXT NOT NULL DEFAULT '',
			token_hash       TEXT NOT NULL,
			status           TEXT NOT NULL DEFAULT 'active',
			quota_bytes      INTEGER NOT NULL DEFAULT 524288000,
			used_bytes       INTEGER NOT NULL DEFAULT 0,
			created_at       TEXT NOT NULL DEFAULT (datetime('now'))
		);

		CREATE TABLE IF NOT EXISTS backups (
			agent_id         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
			timestamp        TEXT NOT NULL,
			encrypted_bytes  INTEGER NOT NULL DEFAULT 0,
			source_file_count INTEGER NOT NULL DEFAULT 0,
			encrypted_sha256 TEXT NOT NULL DEFAULT '',
			s3_key           TEXT NOT NULL,
			manifest_s3_key  TEXT NOT NULL,
			created_at       TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (agent_id, timestamp)
		);

		CREATE INDEX IF NOT EXISTS idx_backups_agent_created
			ON backups(agent_id, created_at);
	`)
	if err != nil {
		return err
	}

	// Migration: add status column to existing databases
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`)

	// Migration: add deleted_at column for soft-delete
	_, _ = db.Exec(`ALTER TABLE backups ADD COLUMN deleted_at TEXT`)

	return nil
}

// ---------------------------------------------------------------------------
// Agent operations
// ---------------------------------------------------------------------------

func (s *SQLiteStore) CreateAgent(a *Agent, tokenHash string) error {
	_, err := s.db.Exec(`
		INSERT INTO agents (id, name, hostname, os, arch, openclaw_version,
			fingerprint, encrypt_tool, public_key, token_hash, status, quota_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.Hostname, a.OS, a.Arch, a.OpenClawVersion,
		a.Fingerprint, a.EncryptTool, a.PublicKey, tokenHash, a.Status, a.QuotaBytes,
	)
	return err
}

func (s *SQLiteStore) LookupAgentByToken(token string) (*Agent, error) {
	h := HashToken(token)
	row := s.db.QueryRow(`
		SELECT id, name, hostname, os, arch, openclaw_version,
			fingerprint, encrypt_tool, public_key, status, quota_bytes, used_bytes, created_at
		FROM agents WHERE token_hash = ?`, h)

	a := &Agent{}
	var createdAt string
	err := row.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch,
		&a.OpenClawVersion, &a.Fingerprint, &a.EncryptTool, &a.PublicKey,
		&a.Status, &a.QuotaBytes, &a.UsedBytes, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	return a, nil
}

func (s *SQLiteStore) GetAgent(id string) (*Agent, error) {
	row := s.db.QueryRow(`
		SELECT id, name, hostname, os, arch, openclaw_version,
			fingerprint, encrypt_tool, public_key, status, quota_bytes, used_bytes, created_at
		FROM agents WHERE id = ?`, id)

	a := &Agent{}
	var createdAt string
	err := row.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch,
		&a.OpenClawVersion, &a.Fingerprint, &a.EncryptTool, &a.PublicKey,
		&a.Status, &a.QuotaBytes, &a.UsedBytes, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	return a, nil
}

func (s *SQLiteStore) RotateAgentToken(agentID, newTokenHash string) error {
	_, err := s.db.Exec(`UPDATE agents SET token_hash = ? WHERE id = ?`, newTokenHash, agentID)
	return err
}

func (s *SQLiteStore) UpdateUsedBytes(agentID string) error {
	_, err := s.db.Exec(`
		UPDATE agents SET used_bytes = (
			SELECT COALESCE(SUM(encrypted_bytes), 0) FROM backups WHERE agent_id = ? AND deleted_at IS NULL
		) WHERE id = ?`, agentID, agentID)
	return err
}

func (s *SQLiteStore) ListAgents(status string) ([]Agent, error) {
	var rows *sql.Rows
	var err error

	if status != "" {
		rows, err = s.db.Query(`
			SELECT id, name, hostname, os, arch, openclaw_version,
				fingerprint, encrypt_tool, public_key, status, quota_bytes, used_bytes, created_at
			FROM agents WHERE status = ? ORDER BY created_at DESC`, status)
	} else {
		rows, err = s.db.Query(`
			SELECT id, name, hostname, os, arch, openclaw_version,
				fingerprint, encrypt_tool, public_key, status, quota_bytes, used_bytes, created_at
			FROM agents ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		var a Agent
		var createdAt string
		if err := rows.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch,
			&a.OpenClawVersion, &a.Fingerprint, &a.EncryptTool, &a.PublicKey,
			&a.Status, &a.QuotaBytes, &a.UsedBytes, &createdAt); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (s *SQLiteStore) CountAgentsByStatus(status string) (int, error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE status = ?`, status)
	var count int
	err := row.Scan(&count)
	return count, err
}

func (s *SQLiteStore) UpdateAgentStatus(id, status string) error {
	res, err := s.db.Exec(`UPDATE agents SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("agent not found: %s", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Backup operations
// ---------------------------------------------------------------------------

func (s *SQLiteStore) CreateBackup(b *Backup) error {
	_, err := s.db.Exec(`
		INSERT INTO backups (agent_id, timestamp, encrypted_bytes, source_file_count,
			encrypted_sha256, s3_key, manifest_s3_key)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.AgentID, b.Timestamp, b.EncryptedBytes, b.SourceFileCount,
		b.EncryptedSHA256, b.S3Key, b.ManifestS3Key,
	)
	if err != nil {
		return err
	}
	return s.UpdateUsedBytes(b.AgentID)
}

func (s *SQLiteStore) ListBackups(agentID string, limit int) ([]Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`
		SELECT agent_id, timestamp, encrypted_bytes, source_file_count,
			encrypted_sha256, s3_key, manifest_s3_key, created_at
		FROM backups WHERE agent_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backups []Backup
	for rows.Next() {
		var b Backup
		var createdAt string
		if err := rows.Scan(&b.AgentID, &b.Timestamp, &b.EncryptedBytes,
			&b.SourceFileCount, &b.EncryptedSHA256, &b.S3Key,
			&b.ManifestS3Key, &createdAt); err != nil {
			return nil, err
		}
		b.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		backups = append(backups, b)
	}
	return backups, rows.Err()
}

func (s *SQLiteStore) CountBackups(agentID string) (int, int64, error) {
	row := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(encrypted_bytes), 0)
		FROM backups WHERE agent_id = ? AND deleted_at IS NULL`, agentID)
	var count int
	var totalBytes int64
	err := row.Scan(&count, &totalBytes)
	return count, totalBytes, err
}

func (s *SQLiteStore) GetBackup(agentID, timestamp string) (*Backup, error) {
	row := s.db.QueryRow(`
		SELECT agent_id, timestamp, encrypted_bytes, source_file_count,
			encrypted_sha256, s3_key, manifest_s3_key, created_at
		FROM backups WHERE agent_id = ? AND timestamp = ? AND deleted_at IS NULL`, agentID, timestamp)

	b := &Backup{}
	var createdAt string
	err := row.Scan(&b.AgentID, &b.Timestamp, &b.EncryptedBytes,
		&b.SourceFileCount, &b.EncryptedSHA256, &b.S3Key,
		&b.ManifestS3Key, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	return b, nil
}

func (s *SQLiteStore) DeleteBackup(agentID, timestamp string) (*Backup, error) {
	b, err := s.GetBackup(agentID, timestamp)
	if err != nil || b == nil {
		return nil, err
	}
	_, err = s.db.Exec(`UPDATE backups SET deleted_at = datetime('now') WHERE agent_id = ? AND timestamp = ? AND deleted_at IS NULL`, agentID, timestamp)
	if err != nil {
		return nil, err
	}
	_ = s.UpdateUsedBytes(agentID)
	return b, nil
}

func (s *SQLiteStore) DeleteAllBackups(agentID string) ([]Backup, error) {
	backups, err := s.ListBackups(agentID, 10000)
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`UPDATE backups SET deleted_at = datetime('now') WHERE agent_id = ? AND deleted_at IS NULL`, agentID)
	if err != nil {
		return nil, err
	}
	_ = s.UpdateUsedBytes(agentID)
	return backups, nil
}

func (s *SQLiteStore) UndeleteBackup(agentID, timestamp string) error {
	res, err := s.db.Exec(`UPDATE backups SET deleted_at = NULL WHERE agent_id = ? AND timestamp = ? AND deleted_at IS NOT NULL`, agentID, timestamp)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("backup not found or not deleted")
	}
	_ = s.UpdateUsedBytes(agentID)
	return nil
}
