package main

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Server
	ListenAddr string

	// Store mode: "sqlite" or "dynamo"
	StoreMode string

	// SQLite (local dev)
	DatabasePath string

	// DynamoDB (Lambda)
	DynamoEndpoint    string
	DynamoAgentsTable string
	DynamoBackupsTable string

	// S3-compatible storage
	S3Endpoint       string
	S3PublicEndpoint string // If set, presigned URLs use this instead of S3Endpoint
	S3Region         string
	S3Bucket         string
	S3AccessKey      string
	S3SecretKey      string
	S3ForcePathStyle bool

	// Token signing
	TokenSecret string

	// API key for admin endpoints (empty = disabled, for local dev)
	AdminAPIKey string

	// Limits
	DefaultQuotaBytes      int64
	RegisterRateLimit      int   // requests per minute per IP
	MaxUploadBytes         int64 // max single upload size in bytes (default 5MB)
	MinBackupIntervalHours int   // minimum hours between backups (default 12)
	MaxBackupsPerAgent     int   // max backups to keep per agent (default 7)
	MaxPendingAgents       int   // max pending registrations (default 100)
	PresignExpiry          time.Duration

	// Retention (free tier defaults)
	RetentionDays    int
	DeleteGraceHours int // hours before soft-deleted backups are purged (default 72)
}

func LoadConfig() *Config {
	// Auto-detect Lambda environment
	storeMode := envOr("STORE_MODE", "sqlite")
	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" && storeMode == "sqlite" {
		storeMode = "dynamo"
	}

	return &Config{
		ListenAddr:         envOr("LISTEN_ADDR", ":8080"),
		StoreMode:          storeMode,
		DatabasePath:       envOr("DATABASE_PATH", "./backup.db"),
		DynamoEndpoint:     envOr("DYNAMO_ENDPOINT", ""),
		DynamoAgentsTable:  envOr("DYNAMO_AGENTS_TABLE", "openclaw-backup-agents"),
		DynamoBackupsTable: envOr("DYNAMO_BACKUPS_TABLE", "openclaw-backup-backups"),
		S3Endpoint:         envOr("S3_ENDPOINT", ""),
		S3PublicEndpoint:   envOr("S3_PUBLIC_ENDPOINT", ""),
		S3Region:           envOr("S3_REGION", "us-east-1"),
		S3Bucket:           envOr("S3_BUCKET", "openclaw-backups"),
		S3AccessKey:        envOr("S3_ACCESS_KEY", ""),
		S3SecretKey:        envOr("S3_SECRET_KEY", ""),
		S3ForcePathStyle:   envOr("S3_FORCE_PATH_STYLE", "false") == "true",
		TokenSecret:        envOr("TOKEN_SECRET", "change-me-in-production"),
		AdminAPIKey:        os.Getenv("ADMIN_API_KEY"),
		DefaultQuotaBytes:      envInt64("DEFAULT_QUOTA_BYTES", 500*1024*1024), // 500 MB
		RegisterRateLimit:      int(envInt64("REGISTER_RATE_LIMIT", 10)),
		MaxUploadBytes:         envInt64("MAX_UPLOAD_BYTES", 5*1024*1024), // 5 MB
		MinBackupIntervalHours: int(envInt64("MIN_BACKUP_INTERVAL_HOURS", 12)),
		MaxBackupsPerAgent:     int(envInt64("MAX_BACKUPS_PER_AGENT", 7)),
		MaxPendingAgents:       int(envInt64("MAX_PENDING_AGENTS", 100)),
		PresignExpiry:          time.Duration(envInt64("PRESIGN_EXPIRY_SECONDS", 900)) * time.Second,
		RetentionDays:          int(envInt64("RETENTION_DAYS", 7)),
		DeleteGraceHours:       int(envInt64("DELETE_GRACE_HOURS", 72)),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

// IsLambda returns true if running inside AWS Lambda.
func (c *Config) IsLambda() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}
