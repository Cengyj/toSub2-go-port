package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func TestHistoricalLoginMaterialsNeverRestoreOrBlockFreshOperations(t *testing.T) {
	for _, operation := range []string{"login", "setup-2fa", "reset-2fa"} {
		for _, stage := range []string{"web_password", "web_email_otp", "email_verified", "callback_ready", "tokens_received"} {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				clearCLISensitiveEnvironment(t)
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Header.Get("Cookie") != "" || r.URL.Path == "/oauth/token" || r.URL.Path == "/api/auth/session" {
						t.Error("old authentication material reached network")
					}
					w.WriteHeader(http.StatusForbidden)
				}))
				defer server.Close()
				cfg := sessionAuditConfig(t.TempDir(), server.URL)
				cfg.CLI.NativeHTTP = true
				vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
				m, _, logger := sessionAuditManager(t, cfg, vault)
				job := sessionAuditImport(t, cfg, m)
				job.Operation = operation
				path := filepath.Join(cfg.OutputRoot, job.ID, "login-checkpoint.json")
				// A deliberately foreign historical transaction cannot bind a new action.
				old := map[string]any{"version": 1, "stage": stage, "email": "foreign@example.test", "authorization_method": "bps", "cookies": []any{map[string]any{"name": "old-session", "value": "synthetic-canary"}}, "oauth": map[string]any{"callbackUrl": "http://localhost:1455/auth/callback?code=old&state=old", "codeVerifier": "old-verifier", "state": "old"}}
				if err := protocol.WritePrivateJSON(path, old); err != nil {
					t.Fatal(err)
				}
				before, _ := os.ReadFile(path)
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				err := makeJobRunnerWithCredentials(cfg, m, logger, vault, true)(ctx, job, func(jobs.JobEvent) {})
				if err == nil || requests.Load() == 0 || strings.Contains(err.Error(), "CHECKPOINT") {
					t.Fatalf("old checkpoint blocked independent login: requests=%d err=%v", requests.Load(), err)
				}
				after, _ := os.ReadFile(path)
				if string(before) != string(after) {
					t.Fatal("runner rewrote legacy session")
				}
			})
		}
	}
}

func TestCallbackCheckpointCannotBeCompletedAfterRestart(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	m, _, logger := sessionAuditManager(t, cfg, vault)
	job := sessionAuditImport(t, cfg, m)
	job.Operation = "resume"
	path := filepath.Join(cfg.OutputRoot, job.ID, "login-checkpoint.json")
	if err := protocol.WritePrivateJSON(path, map[string]any{"version": 1, "email": job.Email, "stage": "callback_ready", "oauth": map[string]any{"callbackUrl": "http://localhost:1455/auth/callback?code=old&state=old", "state": "old", "codeVerifier": "old"}}); err != nil {
		t.Fatal(err)
	}
	err := makeJobRunnerWithCredentials(cfg, m, logger, vault, true)(t.Context(), job, func(jobs.JobEvent) {})
	if err == nil || !strings.Contains(err.Error(), "OAUTH_GRANT_REQUIRED") || calls.Load() != 0 {
		t.Fatal("historical callback used", err)
	}
}
