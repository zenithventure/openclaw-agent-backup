package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// DataStore is the interface for agent and backup persistence.
// Implemented by SQLiteStore (local dev) and DynamoStore (Lambda).
type DataStore interface {
	Close() error

	// Agents
	CreateAgent(a *Agent, tokenHash string) error
	LookupAgentByToken(token string) (*Agent, error)
	GetAgent(id string) (*Agent, error)
	RotateAgentToken(agentID, newTokenHash string) error
	UpdateUsedBytes(agentID string) error
	ListAgents(status string) ([]Agent, error)
	UpdateAgentStatus(id, status string) error

	// Backups
	CreateBackup(b *Backup) error
	ListBackups(agentID string, limit int) ([]Backup, error)
	CountBackups(agentID string) (int, int64, error)
	GetBackup(agentID, timestamp string) (*Backup, error)
	DeleteBackup(agentID, timestamp string) (*Backup, error)
	DeleteAllBackups(agentID string) ([]Backup, error)
}

// ---------------------------------------------------------------------------
// Shared model types
// ---------------------------------------------------------------------------

type Agent struct {
	ID              string
	Name            string
	Hostname        string
	OS              string
	Arch            string
	OpenClawVersion string
	Fingerprint     string
	EncryptTool     string
	PublicKey       string
	Status          string
	QuotaBytes      int64
	UsedBytes       int64
	CreatedAt       time.Time
}

type Backup struct {
	AgentID         string
	Timestamp       string
	EncryptedBytes  int64
	SourceFileCount int64
	EncryptedSHA256 string
	S3Key           string
	ManifestS3Key   string
	CreatedAt       time.Time
}

// ---------------------------------------------------------------------------
// Token helpers (shared across all store implementations)
// ---------------------------------------------------------------------------

// GenerateToken creates a random bearer token and returns (plaintext, sha256_hash).
func GenerateToken() (string, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plain := "ocb_" + hex.EncodeToString(b)
	hash := HashToken(plain)
	return plain, hash, nil
}

// HashToken returns the SHA-256 hex digest of a token string.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// GenerateAgentID creates a random agent ID.
func GenerateAgentID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ag_" + hex.EncodeToString(b), nil
}
