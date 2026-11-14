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
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestProductionLoginIgnoresRetiredGlobalCookieStorage(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Cookie") != "" {
			t.Error("fresh login restored a global Cookie record")
		}
		http.Error(w, "synthetic authentication rejected", http.StatusForbidden)
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	cfg.CLI.NativeHTTP = true
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	m, _, logger := sessionAuditManager(t, cfg, vault)
	job, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"password": "synthetic-password"}})
	if err != nil {
		t.Fatal(err)
	}
	job.Operation = "login"
	// A file at the retired directory path makes any attempted per-account
	// load fail. Fresh login must ignore it and reach the local server.
	path := filepath.Join(cfg.OutputRoot, "account-sessions")
	original := []byte("synthetic invalid retired global Cookie store")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err = makeJobRunnerWithCredentials(cfg, m, logger, vault, true)(ctx, job, func(jobs.JobEvent) {})
	if requests.Load() == 0 || err == nil || strings.Contains(err.Error(), "account-sessions") || strings.Contains(err.Error(), "authorization method mismatch") {
		t.Fatal("fresh login attempted global-session fallback", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(original, after) {
		t.Fatal("ordinary login rewrote retired global Cookie storage", readErr)
	}
}
