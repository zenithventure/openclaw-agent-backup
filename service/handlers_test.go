package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
	if resp.Status != "pending" {
		t.Errorf("expected status pending, got %s", resp.Status)
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
	if agent.Status != "pending" {
		t.Errorf("expected agent status pending, got %s", agent.Status)
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
		Status:     "active",
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
		Status:     "active",
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
	if resp.Status != "active" {
		t.Errorf("expected status active, got %s", resp.Status)
	}
}

func TestRotateToken(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	token, tokenHash, _ := GenerateToken()
	agent := &Agent{
		ID:         "ag_rotate123",
		Name:       "rotate-agent",
		Status:     "active",
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
		Status:     "active",
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

func TestAPIKeyAuth_NoKeyConfigured(t *testing.T) {
	// When no key is configured (empty string), requests pass through
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := APIKeyAuth("", inner)
	req := httptest.NewRequest("POST", "/v1/agents/register", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("expected inner handler to be called when no key configured")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAPIKeyAuth_ValidKey(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := APIKeyAuth("test-secret-key", inner)
	req := httptest.NewRequest("POST", "/v1/agents/register", nil)
	req.Header.Set("X-API-Key", "test-secret-key")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("expected inner handler to be called with valid key")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAPIKeyAuth_MissingKey(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := APIKeyAuth("test-secret-key", inner)
	req := httptest.NewRequest("POST", "/v1/agents/register", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if called {
		t.Error("inner handler should not be called when key is missing")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAPIKeyAuth_WrongKey(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := APIKeyAuth("test-secret-key", inner)
	req := httptest.NewRequest("POST", "/v1/agents/register", nil)
	req.Header.Set("X-API-Key", "wrong-key")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if called {
		t.Error("inner handler should not be called with wrong key")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestGetBackupNotFound(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_notfound",
		Name:       "nf-agent",
		Status:     "active",
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

func TestRequireActive_ActiveAgent(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := RequireActive(inner)

	agent := &Agent{ID: "ag_active", Status: "active"}
	req := httptest.NewRequest("POST", "/v1/backups/upload-url", nil)
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("expected inner handler to be called for active agent")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRequireActive_PendingAgent(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := RequireActive(inner)

	agent := &Agent{ID: "ag_pending", Status: "pending"}
	req := httptest.NewRequest("POST", "/v1/backups/upload-url", nil)
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if called {
		t.Error("inner handler should not be called for pending agent")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "pending" {
		t.Errorf("expected status pending in response, got %s", resp["status"])
	}
}

func TestRequireActive_SuspendedAgent(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := RequireActive(inner)

	agent := &Agent{ID: "ag_suspended", Status: "suspended"}
	req := httptest.NewRequest("POST", "/v1/backups/upload-url", nil)
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if called {
		t.Error("inner handler should not be called for suspended agent")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestAdminApproveAgent(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	// Register creates a pending agent
	agent := &Agent{
		ID:         "ag_approve123",
		Name:       "pending-agent",
		Status:     "pending",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	// Verify agent is pending
	found, _ := h.store.GetAgent(agent.ID)
	if found.Status != "pending" {
		t.Fatalf("expected pending, got %s", found.Status)
	}

	// Approve
	req := httptest.NewRequest("POST", "/v1/admin/agents/ag_approve123/approve", nil)
	req.SetPathValue("id", "ag_approve123")
	w := httptest.NewRecorder()

	h.AdminApproveAgent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify agent is now active
	found, _ = h.store.GetAgent(agent.ID)
	if found.Status != "active" {
		t.Errorf("expected active after approval, got %s", found.Status)
	}
}

func TestAdminSuspendAgent(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_suspend123",
		Name:       "active-agent",
		Status:     "active",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	// Suspend
	req := httptest.NewRequest("POST", "/v1/admin/agents/ag_suspend123/suspend", nil)
	req.SetPathValue("id", "ag_suspend123")
	w := httptest.NewRecorder()

	h.AdminSuspendAgent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify agent is now suspended
	found, _ := h.store.GetAgent(agent.ID)
	if found.Status != "suspended" {
		t.Errorf("expected suspended, got %s", found.Status)
	}
}

func TestAdminListAgents(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	// Create agents with different statuses
	for _, a := range []struct {
		id, name, status string
	}{
		{"ag_list1", "agent-1", "pending"},
		{"ag_list2", "agent-2", "active"},
		{"ag_list3", "agent-3", "pending"},
	} {
		agent := &Agent{
			ID:         a.id,
			Name:       a.name,
			Status:     a.status,
			QuotaBytes: 500 * 1024 * 1024,
		}
		_, tokenHash, _ := GenerateToken()
		h.store.CreateAgent(agent, tokenHash)
	}

	// List all
	req := httptest.NewRequest("GET", "/v1/admin/agents", nil)
	w := httptest.NewRecorder()
	h.AdminListAgents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var all []AdminAgentInfo
	json.NewDecoder(w.Body).Decode(&all)
	if len(all) != 3 {
		t.Errorf("expected 3 agents, got %d", len(all))
	}

	// List pending only
	req = httptest.NewRequest("GET", "/v1/admin/agents?status=pending", nil)
	w = httptest.NewRecorder()
	h.AdminListAgents(w, req)

	var pending []AdminAgentInfo
	json.NewDecoder(w.Body).Decode(&pending)
	if len(pending) != 2 {
		t.Errorf("expected 2 pending agents, got %d", len(pending))
	}
	for _, a := range pending {
		if a.Status != "pending" {
			t.Errorf("expected status pending, got %s", a.Status)
		}
	}
}

func TestAdminApproveAgent_NotFound(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/v1/admin/agents/ag_nonexistent/approve", nil)
	req.SetPathValue("id", "ag_nonexistent")
	w := httptest.NewRecorder()

	h.AdminApproveAgent(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Security hardening tests
// ---------------------------------------------------------------------------

func TestUploadURL_ExceedsMaxUploadBytes(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()
	h.config.MaxUploadBytes = 1024

	agent := &Agent{
		ID:         "ag_maxupload",
		Name:       "max-upload-agent",
		Status:     "active",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	body := `{"timestamp":"2026-02-22T030000Z","encrypted_bytes":2048,"encrypted_sha256":"abc"}`
	req := httptest.NewRequest("POST", "/v1/backups/upload-url", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.UploadURL(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadURL_ZeroBytes(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_zerobytes",
		Name:       "zero-bytes-agent",
		Status:     "active",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	body := `{"timestamp":"2026-02-22T030000Z","encrypted_bytes":0,"encrypted_sha256":"abc"}`
	req := httptest.NewRequest("POST", "/v1/backups/upload-url", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.UploadURL(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadURL_TooFrequent(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()
	h.config.MinBackupIntervalHours = 12

	agent := &Agent{
		ID:         "ag_freq",
		Name:       "freq-agent",
		Status:     "active",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	// Create a recent backup
	h.store.CreateBackup(&Backup{
		AgentID:         agent.ID,
		Timestamp:       "2026-02-22T030000Z",
		EncryptedBytes:  100,
		EncryptedSHA256: "abc",
		S3Key:           agent.ID + "/2026-02-22T030000Z/backup.tar.gz.enc",
		ManifestS3Key:   agent.ID + "/2026-02-22T030000Z/manifest.json",
	})

	body := `{"timestamp":"2026-02-22T040000Z","encrypted_bytes":100,"encrypted_sha256":"def"}`
	req := httptest.NewRequest("POST", "/v1/backups/upload-url", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), agentContextKey, agent)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	h.UploadURL(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadURL_AutoRotation(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()
	h.config.MaxBackupsPerAgent = 2
	h.config.MinBackupIntervalHours = 0 // disable frequency limit for this test

	agent := &Agent{
		ID:         "ag_rotate",
		Name:       "rotate-agent",
		Status:     "active",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	// Create 2 backups manually
	for _, ts := range []string{"2026-02-20T030000Z", "2026-02-21T030000Z"} {
		h.store.CreateBackup(&Backup{
			AgentID:         agent.ID,
			Timestamp:       ts,
			EncryptedBytes:  100,
			EncryptedSHA256: "abc",
			S3Key:           agent.ID + "/" + ts + "/backup.tar.gz.enc",
			ManifestS3Key:   agent.ID + "/" + ts + "/manifest.json",
		})
		// Small delay to ensure ordering
		time.Sleep(10 * time.Millisecond)
	}

	// Verify we have 2
	count, _, _ := h.store.CountBackups(agent.ID)
	if count != 2 {
		t.Fatalf("expected 2 backups, got %d", count)
	}

	// Create a 3rd — should trigger rotation (needs S3 client, but s3 is nil in test)
	// Since UploadURL needs s3, we test the rotation logic directly
	h.store.CreateBackup(&Backup{
		AgentID:         agent.ID,
		Timestamp:       "2026-02-22T030000Z",
		EncryptedBytes:  100,
		EncryptedSHA256: "def",
		S3Key:           agent.ID + "/2026-02-22T030000Z/backup.tar.gz.enc",
		ManifestS3Key:   agent.ID + "/2026-02-22T030000Z/manifest.json",
	})

	// Simulate the auto-rotation logic from UploadURL
	allBackups, _ := h.store.ListBackups(agent.ID, 0)
	if len(allBackups) > h.config.MaxBackupsPerAgent {
		for _, old := range allBackups[h.config.MaxBackupsPerAgent:] {
			h.store.DeleteBackup(agent.ID, old.Timestamp)
		}
		h.store.UpdateUsedBytes(agent.ID)
	}

	// Should now have only 2 visible
	count, _, _ = h.store.CountBackups(agent.ID)
	if count != 2 {
		t.Errorf("expected 2 backups after rotation, got %d", count)
	}

	// Oldest should be soft-deleted
	visible, _ := h.store.ListBackups(agent.ID, 0)
	for _, b := range visible {
		if b.Timestamp == "2026-02-20T030000Z" {
			t.Error("oldest backup should have been rotated out")
		}
	}
}

func TestRegister_MaxPendingExceeded(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()
	h.config.MaxPendingAgents = 2

	// Create 2 pending agents
	for i := 0; i < 2; i++ {
		agent := &Agent{
			ID:         "ag_pending_" + string(rune('a'+i)),
			Name:       "pending-agent",
			Status:     "pending",
			QuotaBytes: 500 * 1024 * 1024,
		}
		_, tokenHash, _ := GenerateToken()
		h.store.CreateAgent(agent, tokenHash)
	}

	// Try to register a 3rd
	body := `{"agent_name":"overflow-agent","hostname":"testhost"}`
	req := httptest.NewRequest("POST", "/v1/agents/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.Register(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteBackup_SoftDelete(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_softdel",
		Name:       "softdel-agent",
		Status:     "active",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	h.store.CreateBackup(&Backup{
		AgentID:         agent.ID,
		Timestamp:       "2026-02-22T030000Z",
		EncryptedBytes:  1024,
		EncryptedSHA256: "abc",
		S3Key:           agent.ID + "/2026-02-22T030000Z/backup.tar.gz.enc",
		ManifestS3Key:   agent.ID + "/2026-02-22T030000Z/manifest.json",
	})

	// Delete (soft)
	_, err := h.store.DeleteBackup(agent.ID, "2026-02-22T030000Z")
	if err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}

	// Should be hidden from list and count
	count, _, _ := h.store.CountBackups(agent.ID)
	if count != 0 {
		t.Errorf("expected 0 visible backups after soft-delete, got %d", count)
	}

	backups, _ := h.store.ListBackups(agent.ID, 0)
	if len(backups) != 0 {
		t.Errorf("expected 0 visible backups in list, got %d", len(backups))
	}

	// Used bytes should be 0
	h.store.UpdateUsedBytes(agent.ID)
	updated, _ := h.store.GetAgent(agent.ID)
	if updated.UsedBytes != 0 {
		t.Errorf("expected used_bytes 0 after soft-delete, got %d", updated.UsedBytes)
	}
}

func TestUndeleteBackup(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_undel",
		Name:       "undel-agent",
		Status:     "active",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	h.store.CreateBackup(&Backup{
		AgentID:         agent.ID,
		Timestamp:       "2026-02-22T030000Z",
		EncryptedBytes:  1024,
		EncryptedSHA256: "abc",
		S3Key:           agent.ID + "/2026-02-22T030000Z/backup.tar.gz.enc",
		ManifestS3Key:   agent.ID + "/2026-02-22T030000Z/manifest.json",
	})

	// Delete
	h.store.DeleteBackup(agent.ID, "2026-02-22T030000Z")

	// Undelete
	err := h.store.UndeleteBackup(agent.ID, "2026-02-22T030000Z")
	if err != nil {
		t.Fatalf("UndeleteBackup: %v", err)
	}

	// Should be visible again
	count, _, _ := h.store.CountBackups(agent.ID)
	if count != 1 {
		t.Errorf("expected 1 backup after undelete, got %d", count)
	}

	b, _ := h.store.GetBackup(agent.ID, "2026-02-22T030000Z")
	if b == nil {
		t.Error("expected backup to be visible after undelete")
	}
}

func TestUndeleteBackup_NotFound(t *testing.T) {
	h, cleanup := setupTestService(t)
	defer cleanup()

	agent := &Agent{
		ID:         "ag_undel404",
		Name:       "undel-nf-agent",
		Status:     "active",
		QuotaBytes: 500 * 1024 * 1024,
	}
	_, tokenHash, _ := GenerateToken()
	h.store.CreateAgent(agent, tokenHash)

	err := h.store.UndeleteBackup(agent.ID, "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent backup undelete")
	}
}

func TestAPIKeyAuth_MultipleKeys(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := APIKeyAuth("key1,key2", inner)

	// key1 should work
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "key1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !called {
		t.Error("key1 should be accepted")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for key1, got %d", w.Code)
	}

	// key2 should work
	called = false
	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "key2")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !called {
		t.Error("key2 should be accepted")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for key2, got %d", w.Code)
	}

	// key3 should fail
	called = false
	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "key3")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if called {
		t.Error("key3 should be rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for key3, got %d", w.Code)
	}
}

func TestAPIKeyAuth_RotatedKey(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	// Simulate rotation: old key + new key
	handler := APIKeyAuth("old-key, new-key", inner)

	// Old key still works
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "old-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !called {
		t.Error("old-key should still be accepted during rotation")
	}

	// New key works
	called = false
	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "new-key")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !called {
		t.Error("new-key should be accepted during rotation")
	}
}
