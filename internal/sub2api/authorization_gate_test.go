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

func TestUploadGateChecksWholeBatchBeforeAnyRemoteRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected remote request", 500)
	}))
	defer server.Close()
	checks := 0
	client := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin", BeforeAuthorizationSync: func(ctx context.Context, account map[string]any) error {
		checks++
		if account["credentials"].(map[string]any)["email"] == "blocked@example.test" {
			return &AuthorizationGateError{Reason: "degraded"}
		}
		return nil
	}})
	_, err := client.Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("allowed@example.test", "synthetic-access-a"), uploadFixtureAccount("blocked@example.test", "synthetic-access-b")))
	var gate *AuthorizationGateError
	if !errors.As(err, &gate) || gate.Reason != "degraded" || checks != 2 || requests.Load() != 0 {
		t.Fatalf("gate result=%v checks=%d remoteRequests=%d", err, checks, requests.Load())
	}
	if IsMutationOutcomeUnknown(err) || !strings.Contains(SafeError(err), "阈值") {
		t.Fatalf("preflight rejection lost safe outcome: %v", err)
	}
}

func TestMonitorCredentialSyncUsesTheSameGateBeforeAnyRemoteRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected remote request", 500)
	}))
	defer server.Close()
	checks := 0
	client := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin", BeforeAuthorizationSync: func(ctx context.Context, account map[string]any) error {
		checks++
		if account["credentials"].(map[string]any)["access_token"] != "synthetic-access" {
			t.Fatal("gate did not receive incoming authorization")
		}
		return &AuthorizationGateError{Reason: "failed"}
	}})
	err := client.SyncAccountCredentialsGuarded(t.Context(), 12, map[string]any{"email": "owner@example.test", "access_token": "synthetic-access"}, "session", nil)
	if err == nil || checks != 1 || requests.Load() != 0 || IsMutationOutcomeUnknown(err) {
		t.Fatalf("repair preflight=%v checks=%d requests=%d", err, checks, requests.Load())
	}
}

func TestAuthorizationGateCancellationNeverStartsProbeOrRemoteMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	checks := 0
	client := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "synthetic-admin", BeforeAuthorizationSync: func(context.Context, map[string]any) error { checks++; return nil }})
	_, err := client.Upload(ctx, uploadFixturePayload(uploadFixtureAccount("owner@example.test", "synthetic-access")))
	if !errors.Is(err, context.Canceled) || IsMutationOutcomeUnknown(err) || checks != 0 {
		t.Fatalf("canceled preflight=%v checks=%d", err, checks)
	}
}

func TestAuthorizationGatePublicMessagesNeverEchoArbitraryReasons(t *testing.T) {
	err := &AuthorizationGateError{Reason: "synthetic-secret-access-token"}
	if strings.Contains(err.Error(), err.Reason) || strings.Contains(SafeError(err), err.Reason) {
		t.Fatal("unknown gate reason escaped into public error")
	}
}
