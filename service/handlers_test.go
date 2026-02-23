package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func setupTestService(t *testing.T) (*Handlers, func()) {
	t.Helper()

	dbPath := t.TempDir() + "/test.db"
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}

	cfg := &Config{
		DefaultQuotaBytes: 500 * 1024 * 1024,
		PresignExpiry:     900,
		RetentionDays:     7,
	}

	h := &Handlers{
		store:  store,
		s3:     nil, // nil for tests that don't need S3
		config: cfg,
	}

	cleanup := func() { store.Close() }
	return h, cleanup
}

func TestRegister(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	body := `{"agent_name":"test-agent","hostname":"testhost","os":"Darwin","arch":"arm64"}`
	req := httptest.NewRequest("POST", "/v1/agents/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.Register(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp RegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.AgentID == "" {
		t.Error("expected non-empty agent_id")
	}
	if resp.Token == "" {
		t.Error("expected non-empty token")
	}
	if resp.QuotaMB != 500 {
		t.Errorf("expected quota 500MB, got %d", resp.QuotaMB)
	}

	// Verify token works for lookup
	agent, err := h.store.LookupAgentByToken(resp.Token)
	if err != nil {
		t.Fatalf("LookupAgentByToken: %v", err)
	}
	if agent == nil {
		t.Fatal("expected agent, got nil")
	}
	if agent.ID != resp.AgentID {
		t.Errorf("expected agent ID %s, got %s", resp.AgentID, agent.ID)
	}
	if agent.Name != "test-agent" {
		t.Errorf("expected name test-agent, got %s", agent.Name)
	}
}

func TestRegisterMissingName(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	body := `{"hostname":"testhost"}`
	req := httptest.NewRequest("POST", "/v1/agents/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.Register(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestListBackupsEmpty(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_test123",
		Name:       "test",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	req := httptest.NewRequest("GET", "/v1/backups", nil)
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.ListBackups(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp ListBackupsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("expected 0 backups, got %d", resp.Count)
	}
}

func TestAgentInfo(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_info123",
		Name:       "info-agent",
		Hostname:   "myhost",
		OS:         "Linux",
		Arch:       "x86_64",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	req := httptest.NewRequest("GET", "/v1/agents/me", nil)
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.AgentInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp AgentInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "info-agent" {
		t.Errorf("expected name info-agent, got %s", resp.Name)
	}
}

func TestRotateToken(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	token, tokenHash, _ := GenerateToken()
	agent := &Agent{
		ID:         "ag_rotate123",
		Name:       "rotate-agent",
		QuotaBytes: 500 * 1024 * 1024,
	}
	h.store.CreateAgent(agent, tokenHash)

	// Verify old token works
	found, _ := h.store.LookupAgentByToken(token)
	if found == nil {
		t.Fatal("old token should work")
	}

	// Rotate
	req := httptest.NewRequest("POST", "/v1/agents/me/rotate-token", nil)
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.RotateToken(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp RotateTokenResponse
	json.NewDecoder(w.Body).Decode(&resp)

	// Old token should no longer work
	found, _ = h.store.LookupAgentByToken(token)
	if found != nil {
		t.Error("old token should be invalidated after rotation")
	}

	// New token should work
	found, _ = h.store.LookupAgentByToken(resp.Token)
	if found == nil {
		t.Error("new token should work")
	}
}

func TestBackupCRUD(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_crud123",
		Name:       "crud-agent",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	b := &Backup{
		AgentID:         agent.ID,
		Timestamp:       "2026-02-22T030000Z",
		EncryptedBytes:  1024,
		SourceFileCount: 10,
		EncryptedSHA256: "abc123",
		S3Key:           agent.ID + "/2026-02-22T030000Z/backup.tar.gz.enc",
		ManifestS3Key:   agent.ID + "/2026-02-22T030000Z/manifest.json",
	}
	if err := h.store.CreateBackup(b); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}

	// Get backup
	req := httptest.NewRequest("GET", "/v1/backups/2026-02-22T030000Z", nil)
	req.SetPathValue("timestamp", "2026-02-22T030000Z")
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.GetBackup(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// List should show 1
	req = httptest.NewRequest("GET", "/v1/backups", nil)
	ctx = context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()

	h.ListBackups(w, req)

	var listResp ListBackupsResponse
	json.NewDecoder(w.Body).Decode(&listResp)
	if listResp.Count != 1 {
		t.Errorf("expected 1 backup, got %d", listResp.Count)
	}

	// Verify used bytes updated
	updated, _ := h.store.GetAgent(agent.ID)
	if updated.UsedBytes != 1024 {
		t.Errorf("expected used_bytes 1024, got %d", updated.UsedBytes)
	}
}

func TestGetBackupNotFound(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_notfound",
		Name:       "nf-agent",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	req := httptest.NewRequest("GET", "/v1/backups/nonexistent", nil)
	req.SetPathValue("timestamp", "nonexistent")
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.GetBackup(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
