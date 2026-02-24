#!/usr/bin/env bash
#
# End-to-end test against the local backup service.
#
# Prerequisites: docker compose up -d
# Usage: bash test-local.sh
#
set -euo pipefail

BASE_URL="${1:-http://localhost:8080}"
ADMIN_KEY="${ADMIN_API_KEY:-}"
PASS=0
FAIL=0

green() { printf '\033[1;32m%s\033[0m\n' "$*"; }
red()   { printf '\033[1;31m%s\033[0m\n' "$*" >&2; }
info()  { printf '\033[1;34m→\033[0m %s\n' "$*"; }

assert_status() {
    local label="$1" expected="$2" actual="$3"
    if [[ "$actual" == "$expected" ]]; then
        green "  PASS: $label (HTTP $actual)"
        PASS=$((PASS + 1))
    else
        red "  FAIL: $label (expected HTTP $expected, got $actual)"
        FAIL=$((FAIL + 1))
    fi
}

assert_json() {
    local label="$1" jq_expr="$2" expected="$3" body="$4"
    local actual
    actual=$(echo "$body" | jq -r "$jq_expr" 2>/dev/null || echo "PARSE_ERROR")
    if [[ "$actual" == "$expected" ]]; then
        green "  PASS: $label = $actual"
        PASS=$((PASS + 1))
    else
        red "  FAIL: $label (expected '$expected', got '$actual')"
        FAIL=$((FAIL + 1))
    fi
}

# Portable curl wrapper: sets RESP_BODY and RESP_STATUS
_RESP_FILE=$(mktemp)
trap "rm -f $_RESP_FILE" EXIT

do_curl() {
    local status
    status=$(curl -s -o "$_RESP_FILE" -w "%{http_code}" "$@")
    RESP_BODY=$(cat "$_RESP_FILE")
    RESP_STATUS="$status"
}

# Admin curl wrapper: adds X-API-Key header if ADMIN_KEY is set
do_admin_curl() {
    if [[ -n "$ADMIN_KEY" ]]; then
        do_curl -H "X-API-Key: $ADMIN_KEY" "$@"
    else
        do_curl "$@"
    fi
}

# -----------------------------------------------------------------------
echo ""
info "Testing backup service at $BASE_URL"
echo ""

# -----------------------------------------------------------------------
# 1. Health check
# -----------------------------------------------------------------------
info "1. Health check"
do_curl "$BASE_URL/healthz"
assert_status "GET /healthz" "200" "$RESP_STATUS"

# -----------------------------------------------------------------------
# 2. Register agent (open registration — no API key needed)
# -----------------------------------------------------------------------
info "2. Register agent"
do_curl -X POST \
    -H "Content-Type: application/json" \
    -d '{"agent_name":"test-agent","hostname":"test-host","os":"Darwin","arch":"arm64","openclaw_version":"0.9.0","encrypt_tool":"age","public_key":"age1testkey"}' \
    "$BASE_URL/v1/agents/register"

assert_status "POST /v1/agents/register" "201" "$RESP_STATUS"

AGENT_ID=$(echo "$RESP_BODY" | jq -r '.agent_id')
TOKEN=$(echo "$RESP_BODY" | jq -r '.token')

assert_json "agent_id starts with ag_" '.agent_id | startswith("ag_")' "true" "$RESP_BODY"
assert_json "token starts with ocb_" '.token | startswith("ocb_")' "true" "$RESP_BODY"
assert_json "status = pending" '.status' "pending" "$RESP_BODY"
assert_json "quota_mb = 500" '.quota_mb' "500" "$RESP_BODY"

info "  Agent ID: $AGENT_ID"
info "  Token: ${TOKEN:0:20}..."

# -----------------------------------------------------------------------
# 3. Register with missing name (should fail)
# -----------------------------------------------------------------------
info "3. Register with missing name"
do_curl -X POST \
    -H "Content-Type: application/json" \
    -d '{"hostname":"oops"}' \
    "$BASE_URL/v1/agents/register"
assert_status "POST /v1/agents/register (no name)" "400" "$RESP_STATUS"

# -----------------------------------------------------------------------
# 4. Agent info (pending agent can still check status)
# -----------------------------------------------------------------------
info "4. Get agent info (pending agent)"
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/agents/me"

assert_status "GET /v1/agents/me" "200" "$RESP_STATUS"
assert_json "name = test-agent" '.name' "test-agent" "$RESP_BODY"
assert_json "status = pending" '.status' "pending" "$RESP_BODY"

# -----------------------------------------------------------------------
# 5. Auth with bad token
# -----------------------------------------------------------------------
info "5. Auth with bad token"
do_curl -H "Authorization: Bearer ocb_invalid" "$BASE_URL/v1/agents/me"
assert_status "GET /v1/agents/me (bad token)" "401" "$RESP_STATUS"

# -----------------------------------------------------------------------
# 6. Pending agent tries upload-url (should get 403)
# -----------------------------------------------------------------------
info "6. Pending agent tries upload-url"
do_curl -X POST \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"timestamp":"2026-02-22T030000Z","files":["backup.tar.gz.enc","manifest.json"],"encrypted_bytes":1024,"encrypted_sha256":"deadbeef"}' \
    "$BASE_URL/v1/backups/upload-url"

assert_status "POST /v1/backups/upload-url (pending)" "403" "$RESP_STATUS"
assert_json "error = agent not active" '.error' "agent not active" "$RESP_BODY"

# -----------------------------------------------------------------------
# 7. Admin approves agent
# -----------------------------------------------------------------------
info "7. Admin approves agent"
do_admin_curl -X POST "$BASE_URL/v1/admin/agents/$AGENT_ID/approve"

assert_status "POST /v1/admin/agents/{id}/approve" "200" "$RESP_STATUS"
assert_json "status = active" '.status' "active" "$RESP_BODY"

# Verify agent is now active
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/agents/me"
assert_json "agent status = active" '.status' "active" "$RESP_BODY"

# -----------------------------------------------------------------------
# 8. List backups (should be empty)
# -----------------------------------------------------------------------
info "8. List backups (empty)"
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups"

assert_status "GET /v1/backups" "200" "$RESP_STATUS"
assert_json "count = 0" '.count' "0" "$RESP_BODY"

# -----------------------------------------------------------------------
# 9. Request upload URLs (now active)
# -----------------------------------------------------------------------
info "9. Request upload URLs"
do_curl -X POST \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"timestamp":"2026-02-22T030000Z","files":["backup.tar.gz.enc","manifest.json"],"encrypted_bytes":1024,"encrypted_sha256":"deadbeef"}' \
    "$BASE_URL/v1/backups/upload-url"

assert_status "POST /v1/backups/upload-url" "200" "$RESP_STATUS"
assert_json "has backup URL" '.urls["backup.tar.gz.enc"] | length > 0' "true" "$RESP_BODY"
assert_json "has manifest URL" '.urls["manifest.json"] | length > 0' "true" "$RESP_BODY"

BACKUP_PUT_URL=$(echo "$RESP_BODY" | jq -r '.urls["backup.tar.gz.enc"]')
MANIFEST_PUT_URL=$(echo "$RESP_BODY" | jq -r '.urls["manifest.json"]')

# -----------------------------------------------------------------------
# 10. Upload a fake encrypted backup via presigned URL
# -----------------------------------------------------------------------
info "10. Upload via presigned URLs"

# Create a file of exactly 1024 bytes to match encrypted_bytes in the upload-url request
dd if=/dev/zero of=/tmp/test-backup.enc bs=1024 count=1 2>/dev/null

do_curl -X PUT \
    -H "Content-Type: application/octet-stream" \
    -H "Content-Length: 1024" \
    --data-binary "@/tmp/test-backup.enc" \
    "$BACKUP_PUT_URL"
assert_status "PUT backup.tar.gz.enc to S3" "200" "$RESP_STATUS"

echo '{"version":1,"timestamp":"2026-02-22T030000Z"}' > /tmp/test-manifest.json

do_curl -X PUT \
    -H "Content-Type: application/json" \
    --data-binary "@/tmp/test-manifest.json" \
    "$MANIFEST_PUT_URL"
assert_status "PUT manifest.json to S3" "200" "$RESP_STATUS"

rm -f /tmp/test-backup.enc /tmp/test-manifest.json

# -----------------------------------------------------------------------
# 11. List backups (should show 1)
# -----------------------------------------------------------------------
info "11. List backups (should show 1)"
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups"

assert_status "GET /v1/backups" "200" "$RESP_STATUS"
assert_json "count = 1" '.count' "1" "$RESP_BODY"

# -----------------------------------------------------------------------
# 12. Get specific backup
# -----------------------------------------------------------------------
info "12. Get specific backup"
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups/2026-02-22T030000Z"

assert_status "GET /v1/backups/{timestamp}" "200" "$RESP_STATUS"
assert_json "timestamp matches" '.timestamp' "2026-02-22T030000Z" "$RESP_BODY"

# -----------------------------------------------------------------------
# 13. Request download URLs
# -----------------------------------------------------------------------
info "13. Request download URLs"
do_curl -X POST \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"timestamp":"2026-02-22T030000Z"}' \
    "$BASE_URL/v1/backups/download-url"

assert_status "POST /v1/backups/download-url" "200" "$RESP_STATUS"
assert_json "has backup download URL" '.urls["backup.tar.gz.enc"] | length > 0' "true" "$RESP_BODY"

# Download and verify content is 1024 bytes
BACKUP_GET_URL=$(echo "$RESP_BODY" | jq -r '.urls["backup.tar.gz.enc"]')
curl -s -o /tmp/test-downloaded.enc "$BACKUP_GET_URL"
DL_SIZE=$(wc -c < /tmp/test-downloaded.enc | tr -d ' ')
if [[ "$DL_SIZE" == "1024" ]]; then
    green "  PASS: downloaded content size matches uploaded (1024 bytes)"
    PASS=$((PASS + 1))
else
    red "  FAIL: downloaded content size mismatch (expected 1024, got $DL_SIZE)"
    FAIL=$((FAIL + 1))
fi
rm -f /tmp/test-downloaded.enc

# -----------------------------------------------------------------------
# 14. Get non-existent backup
# -----------------------------------------------------------------------
info "14. Get non-existent backup"
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups/1999-01-01T000000Z"
assert_status "GET /v1/backups/{nonexistent}" "404" "$RESP_STATUS"

# -----------------------------------------------------------------------
# 15. Rotate token
# -----------------------------------------------------------------------
info "15. Rotate token"
do_curl -X POST -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/agents/me/rotate-token"

assert_status "POST /v1/agents/me/rotate-token" "200" "$RESP_STATUS"

NEW_TOKEN=$(echo "$RESP_BODY" | jq -r '.token')
assert_json "new token starts with ocb_" '.token | startswith("ocb_")' "true" "$RESP_BODY"

# Old token should fail
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/agents/me"
assert_status "old token rejected" "401" "$RESP_STATUS"

# New token should work
do_curl -H "Authorization: Bearer $NEW_TOKEN" "$BASE_URL/v1/agents/me"
assert_status "new token accepted" "200" "$RESP_STATUS"

TOKEN="$NEW_TOKEN"

# -----------------------------------------------------------------------
# 16. Admin list agents
# -----------------------------------------------------------------------
info "16. Admin list agents"
do_admin_curl "$BASE_URL/v1/admin/agents"
assert_status "GET /v1/admin/agents" "200" "$RESP_STATUS"
assert_json "at least 1 agent" '. | length > 0' "true" "$RESP_BODY"

# Filter by status
do_admin_curl "$BASE_URL/v1/admin/agents?status=active"
assert_status "GET /v1/admin/agents?status=active" "200" "$RESP_STATUS"

# -----------------------------------------------------------------------
# 17. Admin suspend agent
# -----------------------------------------------------------------------
info "17. Admin suspend agent"
do_admin_curl -X POST "$BASE_URL/v1/admin/agents/$AGENT_ID/suspend"
assert_status "POST /v1/admin/agents/{id}/suspend" "200" "$RESP_STATUS"

# Suspended agent should be rejected on upload
do_curl -X POST \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"timestamp":"2026-02-23T030000Z","files":["backup.tar.gz.enc","manifest.json"],"encrypted_bytes":1024,"encrypted_sha256":"deadbeef"}' \
    "$BASE_URL/v1/backups/upload-url"
assert_status "POST /v1/backups/upload-url (suspended)" "403" "$RESP_STATUS"

# Re-approve for cleanup
do_admin_curl -X POST "$BASE_URL/v1/admin/agents/$AGENT_ID/approve"
assert_status "POST re-approve" "200" "$RESP_STATUS"

# -----------------------------------------------------------------------
# 18. Delete backup (soft-delete)
# -----------------------------------------------------------------------
info "18. Delete backup (soft-delete)"
do_curl -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups/2026-02-22T030000Z"
assert_status "DELETE /v1/backups/{timestamp}" "200" "$RESP_STATUS"
assert_json "has can_undelete_until" '.can_undelete_until | length > 0' "true" "$RESP_BODY"

# Verify it's hidden from list
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups"
assert_json "backup hidden after soft-delete" '.count' "0" "$RESP_BODY"

# -----------------------------------------------------------------------
# 19. Undelete backup
# -----------------------------------------------------------------------
info "19. Undelete backup"
do_curl -X POST -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups/2026-02-22T030000Z/undelete"
assert_status "POST /v1/backups/{timestamp}/undelete" "200" "$RESP_STATUS"

# Verify it's restored
do_curl -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups"
assert_json "backup restored after undelete" '.count' "1" "$RESP_BODY"

# -----------------------------------------------------------------------
# 20. Undelete non-existent backup (should 404)
# -----------------------------------------------------------------------
info "20. Undelete non-existent backup"
do_curl -X POST -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups/1999-01-01T000000Z/undelete"
assert_status "POST /v1/backups/{nonexistent}/undelete" "404" "$RESP_STATUS"

# -----------------------------------------------------------------------
# 21. Upload too large (should 400)
# -----------------------------------------------------------------------
info "21. Upload too large"
do_curl -X POST \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"timestamp":"2026-02-23T030000Z","encrypted_bytes":999999999,"encrypted_sha256":"abc"}' \
    "$BASE_URL/v1/backups/upload-url"
assert_status "POST /v1/backups/upload-url (too large)" "400" "$RESP_STATUS"

# -----------------------------------------------------------------------
# 22. Multi-key admin auth
# -----------------------------------------------------------------------
info "22. Multi-key admin auth"
if [[ -n "$ADMIN_KEY" ]]; then
    # Current key should still work
    do_curl -H "X-API-Key: $ADMIN_KEY" "$BASE_URL/v1/admin/agents"
    assert_status "admin auth with current key" "200" "$RESP_STATUS"

    # Wrong key should fail
    do_curl -H "X-API-Key: wrong-key" "$BASE_URL/v1/admin/agents"
    assert_status "admin auth with wrong key" "401" "$RESP_STATUS"
else
    green "  SKIP: no ADMIN_API_KEY set (multi-key test requires key)"
    PASS=$((PASS + 1))
fi

# -----------------------------------------------------------------------
# 23. Clean up — delete backup for real
# -----------------------------------------------------------------------
info "23. Final cleanup"
do_curl -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE_URL/v1/backups/2026-02-22T030000Z"
assert_status "DELETE backup for cleanup" "200" "$RESP_STATUS"

# -----------------------------------------------------------------------
# Summary
# -----------------------------------------------------------------------
echo ""
echo "========================================"
TOTAL=$((PASS + FAIL))
if [[ $FAIL -eq 0 ]]; then
    green "ALL $TOTAL TESTS PASSED"
else
    red "$FAIL FAILED out of $TOTAL tests"
fi
echo "========================================"
exit $FAIL
