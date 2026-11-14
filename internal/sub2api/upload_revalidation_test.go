package sub2api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUploadRechecksAuthorizationAfterLookupBeforeAnyMutation(t *testing.T) {
	for _, mode := range []string{"existing", "new", "new-batch"} {
		t.Run(mode, func(t *testing.T) {
			var lookedUp atomic.Bool
			var writes atomic.Int32
			remote := uploadFixtureAccount("owner@example.test", "old-access")
			remote["id"], remote["status"] = 12, "active"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
					lookedUp.Store(true)
					items := []any{}
					if mode == "existing" {
						items = append(items, remote)
					}
					writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": items, "total": len(items)}})
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
					writeUploadFixture(t, w, map[string]any{"data": remote})
				default:
					writes.Add(1)
					writeUploadFixture(t, w, map[string]any{"ok": true})
				}
			}))
			defer server.Close()
			client := New(Config{BaseURL: server.URL, APIKey: "fixture-key", BeforeAuthorizationSync: func(_ context.Context, account map[string]any) error {
				email := AccountEmail(account)
				if lookedUp.Load() && (mode != "new-batch" || email == "blocked@example.test") {
					return &AuthorizationGateError{Reason: "authorization_changed"}
				}
				return nil
			}})
			accounts := []map[string]any{uploadFixtureAccount("owner@example.test", "incoming-access")}
			if mode == "new-batch" {
				accounts = append(accounts, uploadFixtureAccount("blocked@example.test", "incoming-blocked"))
			}
			result, err := client.Upload(t.Context(), uploadFixturePayload(accounts...))
			var gate *AuthorizationGateError
			counts := PublicUploadResult(result)
			if !errors.As(err, &gate) || writes.Load() != 0 || IsMutationOutcomeUnknown(err) || counts["not_attempted"] != float64(len(accounts)) || counts["failed"] != float64(0) || counts["unknown"] != float64(0) {
				t.Fatalf("changed authorization reached mutation: writes=%d counts=%v err=%v", writes.Load(), counts, err)
			}
		})
	}
}

func TestUploadRecheckPreservesEarlierConfirmedProgress(t *testing.T) {
	var writes atomic.Int32
	var blocked atomic.Bool
	first := uploadFixtureAccount("first@example.test", "old-first")
	first["id"], first["status"] = 12, "active"
	second := uploadFixtureAccount("second@example.test", "old-second")
	second["id"], second["status"] = 13, "active"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{first, second}, "total": 2}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
			writeUploadFixture(t, w, map[string]any{"data": first})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/13":
			writeUploadFixture(t, w, map[string]any{"data": second})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
			writes.Add(1)
			blocked.Store(true)
			writeUploadFixture(t, w, map[string]any{"ok": true})
		default:
			writes.Add(1)
			t.Error("later mutation followed a revoked upload decision")
			writeUploadFixture(t, w, map[string]any{"ok": true})
		}
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, APIKey: "fixture-key", BeforeAuthorizationSync: func(_ context.Context, account map[string]any) error {
		if access := account["credentials"].(map[string]any)["access_token"].(string); !strings.HasPrefix(access, "incoming-") {
			t.Error("recheck received remote or merged credentials instead of the incoming grant")
		}
		if blocked.Load() {
			return &AuthorizationGateError{Reason: "degraded"}
		}
		return nil
	}})
	result, err := client.Upload(t.Context(), uploadFixturePayload(
		uploadFixtureAccount("first@example.test", "incoming-first"),
		uploadFixtureAccount("second@example.test", "incoming-second"),
		uploadFixtureAccount("new@example.test", "incoming-new"),
	))
	counts := PublicUploadResult(result)
	var gate *AuthorizationGateError
	if !errors.As(err, &gate) || IsMutationOutcomeUnknown(err) || writes.Load() != 1 || counts["updated"] != float64(1) || counts["success"] != float64(1) || counts["not_attempted"] != float64(2) || counts["failed"] != float64(0) || counts["unknown"] != float64(0) {
		t.Fatalf("pre-send block lost confirmed progress: writes=%d counts=%v err=%v", writes.Load(), counts, err)
	}
}

func TestUploadPreparationReportsSafeStageAndPreservesCause(t *testing.T) {
	for _, stage := range []string{"validation", "lookup", "identity", "http"} {
		t.Run(stage, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				switch stage {
				case "http":
					w.WriteHeader(http.StatusUnauthorized)
				case "identity":
					first := uploadFixtureAccount("owner@example.test", "old-access")
					first["id"] = 12
					second := uploadFixtureAccount("owner@example.test", "old-access")
					second["id"] = 13
					writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{first, second}, "total": 2}})
				default:
					writeUploadFixture(t, w, map[string]any{"data": map[string]any{"unrecognized": "private-canary"}})
				}
			}))
			defer server.Close()
			payload := uploadFixturePayload(uploadFixtureAccount("owner@example.test", "incoming-access"))
			if stage == "validation" {
				payload = map[string]any{"invalid": "private-canary"}
			}
			_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).Upload(t.Context(), payload)
			var preparation *uploadPreparationError
			want := map[string]string{"validation": "校验失败", "lookup": "查询失败", "identity": "身份无法唯一确认", "http": "HTTP 401"}[stage]
			if !errors.As(err, &preparation) || IsMutationOutcomeUnknown(err) || writes.Load() != 0 || !strings.Contains(SafeError(err), want) || !strings.Contains(SafeError(err), "尚未发送授权") || strings.Contains(SafeError(err)+err.Error(), "private-canary") {
				t.Fatalf("preparation stage or safe cause lost: %v", err)
			}
			if stage == "http" && !IsRemoteStatus(err, http.StatusUnauthorized) {
				t.Fatal("typed HTTP cause was lost")
			}
		})
	}
	secret := errors.New("arbitrary-private-canary")
	wrapped := &uploadPreparationError{cause: secret, stage: "arbitrary-private-stage"}
	if !errors.Is(wrapped, secret) || strings.Contains(SafeError(wrapped)+wrapped.Error(), "private") {
		t.Fatal("arbitrary stage or error escaped the closed vocabulary")
	}
}

func TestUploadRejectsBareQueryBaseURLWithoutSendingRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{}}})
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL + "?", APIKey: "fixture-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "incoming-access")))
	if err == nil || requests.Load() != 0 || IsMutationOutcomeUnknown(err) {
		t.Fatalf("bare query reached a remote route: requests=%d err=%v", requests.Load(), err)
	}
}
