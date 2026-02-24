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
