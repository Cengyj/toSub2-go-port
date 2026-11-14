package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestListAuthFilesUsesSafeManagementProtocolAndMinimalMetadata(t *testing.T) {
	filename := authFilename("user@example.com", "workspace-123")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/proxy/v0/management/auth-files" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer management-secret" || r.Header.Get("Accept") != "application/json" {
			t.Error("invalid management list request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"observed_at": "2026-11-13T00:00:00Z", "files": []any{
			map[string]any{"name": filename, "type": "codex", "provider": "codex", "status": "error", "status_message": "unauthorized (refresh token invalid)", "email": "User@Example.com", "source": "file", "id_token": map[string]any{"chatgpt_account_id": "workspace-123", "plan_type": "team"}, "access_token": "private-access", "path": "/private/auth.json", "private_data": "private-value"},
			map[string]any{"name": "someone-elses.json", "type": "codex", "status": "error", "status_message": "invalid_grant", "email": "user@example.com", "id_token": "private-token"},
		}})
	}))
	defer server.Close()
	files, err := New(Config{BaseURL: server.URL + "/proxy/v0/management", APIKey: "management-secret"}).ListAuthFiles(t.Context())
	if err != nil || len(files) != 1 || calls.Load() != 1 {
		t.Fatalf("files=%+v; error=%v; requests=%d", files, err, calls.Load())
	}
	got := files[0]
	if got.Filename != filename || got.Email != "user@example.com" || got.AccountID != "workspace-123" || got.PlanType != "team" || !got.NeedsRelogin || !got.MatchesIdentity("User@Example.com", "workspace-123") {
		t.Fatalf("unexpected reconciliation metadata: %+v", got)
	}
	output, _ := json.Marshal(files)
	for _, secret := range []string{"management-secret", "private-access", "private-token", "/private/auth.json", "private-value", "refresh token invalid"} {
		if strings.Contains(string(output), secret) {
			t.Fatal("remote listing exposed unnecessary private data")
		}
	}
}

func TestRemoteNeedsReloginRequiresUnambiguousAuthorizationEvidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		change  func(*remoteAuthWire)
		want    bool
	}{
		{"invalid-refresh", "unauthorized (refresh token invalid)", nil, true},
		{"expired-access", "token expired", nil, true},
		{"unauthorized", "unauthorized", nil, true},
		{"reused-refresh", `token refresh failed with status 400: {"error":{"code":"refresh_token_reused"}}`, nil, true},
		{"invalid-grant", `token refresh failed with status 401: {"error":"invalid_grant"}`, nil, true},
		{"invalidated-refresh", "refresh_token_invalidated", nil, true},
		{"optional-sign-in-field", "", func(r *remoteAuthWire) { r.RequiresSignIn = true }, true},
		{"optional-refresh-error", "", func(r *remoteAuthWire) {
			r.RefreshError.Code = "refresh_token_expired"
			r.RefreshError.HTTPStatus = 401
		}, true},
		{"active-with-old-error", "invalid_grant", func(r *remoteAuthWire) { r.Status = "active" }, false},
		{"disk-only", "", func(r *remoteAuthWire) { r.Status = "" }, false},
		{"disabled", "invalid_grant", func(r *remoteAuthWire) { r.Disabled = true }, false},
		{"disabled-status", "invalid_grant", func(r *remoteAuthWire) { r.Status = "disabled" }, false},
		{"runtime-only", "invalid_grant", func(r *remoteAuthWire) { r.RuntimeOnly = true }, false},
		{"in-memory", "invalid_grant", func(r *remoteAuthWire) { r.Source = "memory" }, false},
		{"quota", "insufficient_quota", nil, false},
		{"429", "status 429: invalid_grant", nil, false},
		{"quota-and-auth", "quota exceeded after invalid_grant", nil, false},
		{"quota-flag", "invalid_grant", func(r *remoteAuthWire) { r.QuotaBlocked = true }, false},
		{"quota-recovery", "invalid_grant", func(r *remoteAuthWire) { r.RecoveryStage = "quota_blocked" }, false},
		{"5xx", "status 503: invalid_grant", nil, false},
		{"5xx-field", "invalid_grant", func(r *remoteAuthWire) { r.RefreshError.HTTPStatus = 502 }, false},
		{"network", "network timeout invalid_grant", nil, false},
		{"banned", "account banned invalid_grant", nil, false},
		{"deactivated", "account_deactivated invalid_grant", nil, false},
		{"forbidden", "403 unauthorized", nil, false},
		{"ambiguous-refresh", "refresh_outcome_unknown", nil, false},
		{"unknown", "something failed", nil, false},
		{"no-error", "", nil, false},
		{"unrelated-error-substring", "not_invalid_grant", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := remoteAuthWire{Status: "error", Source: "file", StatusMessage: test.message}
			if test.change != nil {
				test.change(&raw)
			}
			if got := remoteNeedsRelogin(raw); got != test.want {
				t.Fatalf("NeedsRelogin=%v; want=%v", got, test.want)
			}
		})
	}
}

func TestRemoteAuthIdentityAndPlanEvidence(t *testing.T) {
	filename := authFilename("user@example.com", "workspace-123")
	for _, test := range []struct {
		name   string
		fields map[string]any
		count  int
		match  bool
		plan   string
	}{
		{"missing-optional-identity", nil, 1, true, ""},
		{"id-token-metadata", map[string]any{"id_token": map[string]any{"chatgpt_account_id": "workspace-123", "plan_type": "plus"}}, 1, true, "plus"},
		{"top-level-metadata", map[string]any{"email": "USER@EXAMPLE.COM", "account_id": "workspace-123", "plan_type": "TEAM"}, 1, true, "team"},
		{"conflicting-email", map[string]any{"email": "user@example.com", "id_token": map[string]any{"email": "other@example.com"}}, 0, false, ""},
		{"conflicting-account", map[string]any{"account_id": "workspace-123", "id_token": map[string]any{"chatgpt_account_id": "other"}}, 0, false, ""},
		{"conflicting-plan", map[string]any{"plan_type": "free", "id_token": map[string]any{"plan_type": "team"}}, 0, false, ""},
		{"mismatched-email", map[string]any{"email": "other@example.com"}, 1, false, ""},
		{"mismatched-account", map[string]any{"account_id": "other"}, 1, false, ""},
		{"other-provider", map[string]any{"provider": "claude"}, 0, false, ""},
		{"invalid-email", map[string]any{"email": "not an email"}, 0, false, ""},
		{"non-owned-file", map[string]any{"name": "user@example.com.json"}, 0, false, ""},
		{"traversal", map[string]any{"name": "../" + filename}, 0, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := map[string]any{"name": filename, "type": "codex", "status": "error", "status_message": "token expired"}
			for k, v := range test.fields {
				entry[k] = v
			}
			data, _ := json.Marshal(map[string]any{"files": []any{entry}})
			files, err := parseAuthList(data)
			if err != nil || len(files) != test.count {
				t.Fatalf("files=%+v; err=%v", files, err)
			}
			if len(files) > 0 && (files[0].MatchesIdentity("user@example.com", "workspace-123") != test.match || files[0].PlanType != test.plan || files[0].MatchesIdentity("user@example.com", "other-workspace")) {
				t.Fatalf("identity/plan check failed: %+v", files[0])
			}
		})
	}
}

func TestListAuthFilesRejectsMalformedAmbiguousOrPrivateFailureResponses(t *testing.T) {
	file := fmt.Sprintf(`{"name":%q,"type":"codex","status":"error","status_message":"token expired"}`, authFilename("user@example.com", "workspace-123"))
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"redirect", 307, "private-response"},
		{"unauthorized", 401, "private-response"},
		{"server-error", 503, "private-response"},
		{"partial", 206, `{"files":[]}`},
		{"invalid-json", 200, "private-response"},
		{"missing-files", 200, `{}`},
		{"null-files", 200, `{"files":null}`},
		{"wrong-container", 200, `{"files":{}}`},
		{"invalid-entry", 200, `{"files":[null]}`},
		{"duplicate-keys", 200, `{"files":[],"files":[` + file + `]}`},
		{"folded-duplicate", 200, `{"files":[],"Files":[` + file + `]}`},
		{"duplicate-file", 200, `{"files":[` + file + `,` + file + `]}`},
		{"trailing-document", 200, `{"files":[]} {}`},
		{"large-response", 200, strings.Repeat("x", maxAuthListBytes+1)},
		{"folded-disabled", 200, `{"files":[` + strings.TrimSuffix(file, "}") + `,"disabled":false,"Disabled":true}]}`},
		{"folded-identity", 200, `{"files":[` + strings.TrimSuffix(file, "}") + `,"id_token":{"chatgpt_account_id":"one","CHATGPT_ACCOUNT_ID":"two"}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/replay?secret=private-redirect")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			files, err := New(Config{BaseURL: server.URL, APIKey: "management-secret", Timeout: time.Second}).ListAuthFiles(t.Context())
			if err == nil || len(files) != 0 || calls.Load() != 1 {
				t.Fatalf("files=%+v; err=%v; requests=%d", files, err, calls.Load())
			}
			for _, secret := range []string{"private-response", "private-redirect", "management-secret", server.URL} {
				if strings.Contains(err.Error(), secret) || strings.Contains(SafeListError(err), secret) {
					t.Fatal("status-read error exposed private data")
				}
			}
		})
	}
}

func TestListAuthFilesEmptyDiskAndCanceledReads(t *testing.T) {
	files, err := parseAuthList([]byte(`{"files":[]}`))
	if err != nil || files == nil || len(files) != 0 {
		t.Fatalf("empty list=%+v; error=%v", files, err)
	}
	data, _ := json.Marshal(map[string]any{"files": []any{map[string]any{"name": authFilename("user@example.com", "workspace-123"), "type": "codex", "email": "user@example.com"}}})
	files, err = parseAuthList(data)
	if err != nil || len(files) != 1 || files[0].NeedsRelogin {
		t.Fatalf("disk-only listing=%+v; error=%v", files, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "key"}).ListAuthFiles(ctx)
	if !errors.Is(err, context.Canceled) || SafeListError(err) != "CPA 状态读取已取消" {
		t.Fatalf("canceled error=%v", err)
	}
	var nilClient *Client
	_, err = nilClient.ListAuthFiles(nil)
	if !errors.Is(err, ErrInvalidBaseURL) {
		t.Fatalf("nil client error=%v", err)
	}
}
