package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func TestInvalidOAuthResultIDCannotReadFallbackOrConnect(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := credentials.NewMemoryStore()
	m, st, logger := sessionAuditManager(t, cfg, vault)
	job, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, Operation: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	m.Cancel(job.ID)
	job.OAuthResultID = "../invalid"
	path := filepath.Join(cfg.OutputRoot, job.ID, "oauth-grant-invalid-result.json")
	if err := protocol.WriteSavedOAuthGrant(path, cliTestGrant(job.Email, "chatgpt")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	err = makeJobRunnerWithCredentials(cfg, m, logger, vault, true)(t.Context(), job, func(jobs.JobEvent) {})
	if err == nil || !strings.Contains(err.Error(), "OAUTH_RESULT_ID_INVALID") || requests.Load() != 0 {
		t.Fatal("invalid result identifier reached a result file or transport", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{"jobs": []any{map[string]any{
		"id": job.ID, "email": job.Email, "operation": "resume", "status": "queued", "oauth_result_id": job.OAuthResultID,
	}}}); err != nil {
		t.Fatal(err)
	}
	restored := jobs.NewManager(t.Context(), 1, st, logger)
	defer restored.Shutdown(context.Background())
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := restored.Get(job.ID)
	after, err := os.ReadFile(path)
	if current.Status != jobs.StatusFailed || current.Stage != "invalid_oauth_result" || current.ResultSaved || requests.Load() != 0 || err != nil || !bytes.Equal(before, after) {
		t.Fatal("restart read a fallback result for an invalid identifier")
	}
}

func TestFailedFreshLoginNeverCompletesOlderIssuedTokens(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/oauth/token" {
			t.Error("failed independent login exchanged a token")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	cfg.CLI.NativeHTTP = true
	vault := credentials.NewMemoryStore()
	m, st, logger := sessionAuditManager(t, cfg, vault)
	job, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(cfg.OutputRoot, job.ID, "oauth-grant.json")
	old := cliTestGrant(job.Email, "chatgpt")
	if err := protocol.WriteSavedOAuthGrant(oldPath, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(oldPath)
	fresh, err := m.QueueOperation(job.ID, "relogin", map[string]any{"authorizationMethod": "bps"})
	if err != nil {
		t.Fatal("older grant blocked an independent provider selection", err)
	}
	m.SetRunner(makeJobRunnerWithCredentials(cfg, m, logger, vault, true))
	m.Start()
	if err := sessionAuditWait(t, m); err == nil || requests.Load() == 0 {
		t.Fatal("independent login did not fail against the local endpoint")
	}
	firstCalls := requests.Load()
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored := jobs.NewManager(t.Context(), 1, st, logger)
	defer restored.Shutdown(context.Background())
	restored.SetCredentialStore(vault)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := restored.Get(job.ID)
	if current.Operation != "relogin" || current.Status == "resume_available" || current.ResultSaved || current.OAuthResultID != fresh.OAuthResultID {
		t.Fatal("restart attributed the old issued grant to the failed login")
	}
	retried, err := restored.Retry(job.ID)
	if err != nil || retried.Operation == "resume" || retried.OAuthResultID == current.OAuthResultID {
		t.Fatal("retry selected old tokens instead of an independent operation", err)
	}
	restored.SetRunner(makeJobRunnerWithCredentials(cfg, restored, logger, vault, true))
	restored.Start()
	if err := sessionAuditWait(t, restored); err == nil || requests.Load() <= firstCalls {
		t.Fatal("retry falsely completed with the older tokens")
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")); !os.IsNotExist(err) {
		t.Fatal("failed login created a final authorization output")
	}
	after, err := os.ReadFile(oldPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("independent login removed or changed old issued tokens")
	}
	// The older pure result remains explicitly usable without authentication.
	calls := requests.Load()
	_, err = protocol.FinalizeSavedOAuthGrant(t.Context(), old, protocol.FlowOptions{
		Email: old.Email, AuthorizationMethod: old.AuthorizationMethod, AuthBase: old.AuthBase, ChatGPTBase: old.ChatGPTBase,
		ClientID: old.ClientID, RedirectURI: old.RedirectURI, OutputPath: filepath.Join(t.TempDir(), "older-result.json"),
	})
	if err != nil || requests.Load() != calls {
		t.Fatal("preserved issued tokens could not explicitly finish offline", err)
	}
}
