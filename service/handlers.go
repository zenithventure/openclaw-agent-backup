package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
)

type Handlers struct {
	store  DataStore
	s3     *S3Client
	config *Config
}

// ---------------------------------------------------------------------------
// POST /v1/agents/register
// ---------------------------------------------------------------------------

type RegisterRequest struct {
	AgentName      string `json:"agent_name"`
	Hostname       string `json:"hostname"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	OpenClawVersion string `json:"openclaw_version"`
	Fingerprint    string `json:"machine_fingerprint"`
	EncryptTool    string `json:"encrypt_tool"`
	PublicKey      string `json:"public_key"`
}

type RegisterResponse struct {
	AgentID      string `json:"agent_id"`
	Token        string `json:"token"`
	QuotaMB      int64  `json:"quota_mb"`
	BackupPrefix string `json:"backup_prefix"`
}

func (h *Handlers) Register(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.AgentName == "" {
		jsonError(w, "agent_name is required", http.StatusBadRequest)
		return
	}

	agentID, err := GenerateAgentID()
	if err != nil {
		log.Printf("ERROR: generate agent ID: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	token, tokenHash, err := GenerateToken()
	if err != nil {
		log.Printf("ERROR: generate token: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	agent := &Agent{
		ID:              agentID,
		Name:            req.AgentName,
		Hostname:        req.Hostname,
		OS:              req.OS,
		Arch:            req.Arch,
		OpenClawVersion: req.OpenClawVersion,
		Fingerprint:     req.Fingerprint,
		EncryptTool:     req.EncryptTool,
		PublicKey:        req.PublicKey,
		QuotaBytes:      h.config.DefaultQuotaBytes,
	}

	if err := h.store.CreateAgent(agent, tokenHash); err != nil {
		log.Printf("ERROR: create agent: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("registered agent %s (%s) from %s", agentID, req.AgentName, req.Hostname)

	jsonResponse(w, http.StatusCreated, RegisterResponse{
		AgentID:      agentID,
		Token:        token,
		QuotaMB:      h.config.DefaultQuotaBytes / (1024 * 1024),
		BackupPrefix: agentID + "/",
	})
}

// ---------------------------------------------------------------------------
// POST /v1/backups/upload-url
// ---------------------------------------------------------------------------

type UploadURLRequest struct {
	Timestamp       string   `json:"timestamp"`
	Files           []string `json:"files"`
	EncryptedBytes  int64    `json:"encrypted_bytes"`
	EncryptedSHA256 string   `json:"encrypted_sha256"`
}

type UploadURLResponse struct {
	URLs      map[string]string `json:"urls"`
	ExpiresIn int               `json:"expires_in"`
}

func (h *Handlers) UploadURL(w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())

	var req UploadURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Timestamp == "" {
		jsonError(w, "timestamp is required", http.StatusBadRequest)
		return
	}

	// Check quota
	if agent.UsedBytes+req.EncryptedBytes > agent.QuotaBytes {
		jsonError(w, fmt.Sprintf("quota exceeded: used %d + new %d > quota %d bytes",
			agent.UsedBytes, req.EncryptedBytes, agent.QuotaBytes), http.StatusForbidden)
		return
	}

	prefix := agent.ID + "/" + req.Timestamp + "/"
	urls := make(map[string]string)

	// Default file list if not provided
	if len(req.Files) == 0 {
		req.Files = []string{"backup.tar.gz.enc", "manifest.json"}
	}

	for _, file := range req.Files {
		key := prefix + file
		contentType := "application/octet-stream"
		if file == "manifest.json" {
			contentType = "application/json"
		}

		url, err := h.s3.PresignPut(r.Context(), key, contentType)
		if err != nil {
			log.Printf("ERROR: presign PUT %s: %v", key, err)
			jsonError(w, "failed to generate upload URL", http.StatusInternalServerError)
			return
		}
		urls[file] = url
	}

	// Record the backup metadata
	backupS3Key := prefix + "backup.tar.gz.enc"
	manifestS3Key := prefix + "manifest.json"

	backup := &Backup{
		AgentID:         agent.ID,
		Timestamp:       req.Timestamp,
		EncryptedBytes:  req.EncryptedBytes,
		SourceFileCount: 0, // updated when manifest is available
		EncryptedSHA256: req.EncryptedSHA256,
		S3Key:           backupS3Key,
		ManifestS3Key:   manifestS3Key,
	}

	if err := h.store.CreateBackup(backup); err != nil {
		log.Printf("ERROR: create backup record: %v", err)
		jsonError(w, "failed to record backup", http.StatusInternalServerError)
		return
	}

	jsonResponse(w, http.StatusOK, UploadURLResponse{
		URLs:      urls,
		ExpiresIn: int(h.config.PresignExpiry.Seconds()),
	})
}

// ---------------------------------------------------------------------------
// GET /v1/backups
// ---------------------------------------------------------------------------

type ListBackupsResponse struct {
	Backups    []BackupInfo `json:"backups"`
	Count      int          `json:"count"`
	UsedBytes  int64        `json:"used_bytes"`
	QuotaBytes int64        `json:"quota_bytes"`
}

type BackupInfo struct {
	Timestamp       string `json:"timestamp"`
	EncryptedBytes  int64  `json:"encrypted_bytes"`
	SourceFileCount int64  `json:"source_file_count"`
	EncryptedSHA256 string `json:"encrypted_sha256"`
	CreatedAt       string `json:"created_at"`
}

func (h *Handlers) ListBackups(w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())

	countOnly := r.URL.Query().Get("count_only") == "true"
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	count, usedBytes, err := h.store.CountBackups(agent.ID)
	if err != nil {
		log.Printf("ERROR: count backups: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if countOnly {
		jsonResponse(w, http.StatusOK, ListBackupsResponse{
			Backups:    []BackupInfo{},
			Count:      count,
			UsedBytes:  usedBytes,
			QuotaBytes: agent.QuotaBytes,
		})
		return
	}

	backups, err := h.store.ListBackups(agent.ID, limit)
	if err != nil {
		log.Printf("ERROR: list backups: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	infos := make([]BackupInfo, len(backups))
	for i, b := range backups {
		infos[i] = BackupInfo{
			Timestamp:       b.Timestamp,
			EncryptedBytes:  b.EncryptedBytes,
			SourceFileCount: b.SourceFileCount,
			EncryptedSHA256: b.EncryptedSHA256,
			CreatedAt:       b.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}
	}

	jsonResponse(w, http.StatusOK, ListBackupsResponse{
		Backups:    infos,
		Count:      count,
		UsedBytes:  usedBytes,
		QuotaBytes: agent.QuotaBytes,
	})
}

// ---------------------------------------------------------------------------
// GET /v1/backups/{timestamp}
// ---------------------------------------------------------------------------

func (h *Handlers) GetBackup(w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	timestamp := r.PathValue("timestamp")

	backup, err := h.store.GetBackup(agent.ID, timestamp)
	if err != nil {
		log.Printf("ERROR: get backup: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if backup == nil {
		jsonError(w, "backup not found", http.StatusNotFound)
		return
	}

	jsonResponse(w, http.StatusOK, BackupInfo{
		Timestamp:       backup.Timestamp,
		EncryptedBytes:  backup.EncryptedBytes,
		SourceFileCount: backup.SourceFileCount,
		EncryptedSHA256: backup.EncryptedSHA256,
		CreatedAt:       backup.CreatedAt.Format("2006-01-02T15:04:05Z"),
	})
}

// ---------------------------------------------------------------------------
// POST /v1/backups/download-url
// ---------------------------------------------------------------------------

type DownloadURLRequest struct {
	Timestamp string `json:"timestamp"`
}

type DownloadURLResponse struct {
	URLs      map[string]string `json:"urls"`
	ExpiresIn int               `json:"expires_in"`
}

func (h *Handlers) DownloadURL(w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())

	var req DownloadURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Timestamp == "" {
		jsonError(w, "timestamp is required", http.StatusBadRequest)
		return
	}

	backup, err := h.store.GetBackup(agent.ID, req.Timestamp)
	if err != nil {
		log.Printf("ERROR: get backup: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if backup == nil {
		jsonError(w, "backup not found", http.StatusNotFound)
		return
	}

	urls := make(map[string]string)

	backupURL, err := h.s3.PresignGet(r.Context(), backup.S3Key)
	if err != nil {
		log.Printf("ERROR: presign GET backup: %v", err)
		jsonError(w, "failed to generate download URL", http.StatusInternalServerError)
		return
	}
	urls["backup.tar.gz.enc"] = backupURL

	manifestURL, err := h.s3.PresignGet(r.Context(), backup.ManifestS3Key)
	if err != nil {
		log.Printf("ERROR: presign GET manifest: %v", err)
		jsonError(w, "failed to generate download URL", http.StatusInternalServerError)
		return
	}
	urls["manifest.json"] = manifestURL

	jsonResponse(w, http.StatusOK, DownloadURLResponse{
		URLs:      urls,
		ExpiresIn: int(h.config.PresignExpiry.Seconds()),
	})
}

// ---------------------------------------------------------------------------
// DELETE /v1/backups/{timestamp}
// ---------------------------------------------------------------------------

func (h *Handlers) DeleteBackup(w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	timestamp := r.PathValue("timestamp")

	backup, err := h.store.DeleteBackup(agent.ID, timestamp)
	if err != nil {
		log.Printf("ERROR: delete backup: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if backup == nil {
		jsonError(w, "backup not found", http.StatusNotFound)
		return
	}

	h.s3.DeleteBackupObjects(r.Context(), backup)

	jsonResponse(w, http.StatusOK, map[string]string{"deleted": timestamp})
}

// ---------------------------------------------------------------------------
// DELETE /v1/backups
// ---------------------------------------------------------------------------

func (h *Handlers) DeleteAllBackups(w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())

	backups, err := h.store.DeleteAllBackups(agent.ID)
	if err != nil {
		log.Printf("ERROR: delete all backups: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	for i := range backups {
		h.s3.DeleteBackupObjects(r.Context(), &backups[i])
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"deleted_count": len(backups),
	})
}

// ---------------------------------------------------------------------------
// GET /v1/agents/me
// ---------------------------------------------------------------------------

type AgentInfoResponse struct {
	AgentID         string `json:"agent_id"`
	Name            string `json:"name"`
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	OpenClawVersion string `json:"openclaw_version"`
	EncryptTool     string `json:"encrypt_tool"`
	QuotaBytes      int64  `json:"quota_bytes"`
	UsedBytes       int64  `json:"used_bytes"`
	CreatedAt       string `json:"created_at"`
}

func (h *Handlers) AgentInfo(w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())

	// Refresh used bytes
	h.store.UpdateUsedBytes(agent.ID)
	updated, _ := h.store.GetAgent(agent.ID)
	if updated != nil {
		agent = updated
	}

	jsonResponse(w, http.StatusOK, AgentInfoResponse{
		AgentID:         agent.ID,
		Name:            agent.Name,
		Hostname:        agent.Hostname,
		OS:              agent.OS,
		Arch:            agent.Arch,
		OpenClawVersion: agent.OpenClawVersion,
		EncryptTool:     agent.EncryptTool,
		QuotaBytes:      agent.QuotaBytes,
		UsedBytes:       agent.UsedBytes,
		CreatedAt:       agent.CreatedAt.Format("2006-01-02T15:04:05Z"),
	})
}

// ---------------------------------------------------------------------------
// POST /v1/agents/me/rotate-token
// ---------------------------------------------------------------------------

type RotateTokenResponse struct {
	Token string `json:"token"`
}

func (h *Handlers) RotateToken(w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())

	newToken, newHash, err := GenerateToken()
	if err != nil {
		log.Printf("ERROR: generate token: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.store.RotateAgentToken(agent.ID, newHash); err != nil {
		log.Printf("ERROR: rotate token: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("rotated token for agent %s", agent.ID)

	jsonResponse(w, http.StatusOK, RotateTokenResponse{
		Token: newToken,
	})
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
