package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

func TestFourIndependentTOTPResetsUseFreshLoginAndDeduplicateIntents(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditImport(t, cfg, m)
	m.SetRunner(makeJobRunnerWithCredentials(cfg, m, logger, vault, true))
	m.Start()
	previousOperation := ""
	for round := 1; round <= 4; round++ {
		seed := []string{"JBSWY3DPEHPK3PXP", "MFRGGZDFMZTWQ2LK", "ONSWG4TFOQWWK4ZB", "GEZDGNBVGY3TQOJQ"}[round-1]
		remote.mu.Lock()
		remote.nextFactor, remote.nextSecret, remote.enrollmentSession = fmt.Sprintf("new-factor-%d", round), seed, fmt.Sprintf("enrollment-%d", round)
		remote.mu.Unlock()
		values := map[string]any{"totpIntentId": fmt.Sprintf("fresh-reset-round-%d", round)}
		if _, err := m.QueueOperation(sessionAuditJobID, "reset-2fa", values); err != nil {
			t.Fatal(err)
		}
		if _, err := m.QueueOperation(sessionAuditJobID, "reset-2fa", values); err != nil {
			t.Fatal("same intent was not idempotent", err)
		}
		if err := sessionAuditWait(t, m); err != nil {
			t.Fatal(err)
		}
		state := remote.snapshot()
		if state.loginRequests != round || state.disableCalls != round || state.enrollCalls != round || state.activateCalls != round || state.infoReads != round {
			t.Fatalf("round %d repeated mutation or added a confirmation read: %+v", round, state)
		}
		result, err := protocol.ReadTOTPResetCompletion(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "totp-setup-result.json"), sessionAuditEmail)
		operation, _ := result["operation_id"].(string)
		if err != nil || operation == "" || operation == previousOperation || result["credential_persisted"] != true {
			t.Fatal("independent operation did not commit new bound evidence", err)
		}
		previousOperation = operation
		saved, err := vault.Load(sessionAuditEmail)
		if err != nil || saved.TOTPSecret != seed || saved.TOTPResetOperationID != operation || saved.TOTPResetPending {
			t.Fatal("bound key commit incomplete", err)
		}
		assertNoTaskCheckpoint(t, filepath.Join(cfg.OutputRoot, sessionAuditJobID, "login-checkpoint.json"))
		if _, handled, err := m.ReplayTOTPIntent(sessionAuditJobID, values); err != nil || !handled {
			t.Fatal("completed duplicate intent admitted", err)
		}
		if after := remote.snapshot(); after != state {
			t.Fatal("duplicate completed intent changed remote state")
		}
	}
}
