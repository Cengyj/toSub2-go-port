package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
)

type resetCommitIdentityReadbackVault struct {
	*credentials.MemoryStore
	corrupt atomic.Bool
}

func (v *resetCommitIdentityReadbackVault) Save(email string, value credentials.Credentials) error {
	if value.TOTPSecret == sessionAuditNew && !value.TOTPResetPending && v.corrupt.Swap(false) {
		value.TOTPResetOperationID = "synthetic-conflicting-operation"
	}
	return v.MemoryStore.Save(email, value)
}

func TestTOTPResetCommitReadbackRejectsChangedOperationIdentity(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	vault := &resetCommitIdentityReadbackVault{MemoryStore: credentials.NewMemoryStore()}
	vault.corrupt.Store(true)
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditPrepare(t, cfg, m)
	if err := sessionAuditExecute(t, cfg, m, logger, vault); err == nil {
		t.Fatal("readback accepted another operation identity")
	}
	journal := sessionAuditJournal(t, cfg)
	if journal["activation_verified"] != true || journal["credential_persisted"] != false {
		t.Fatal("conflicting vault readback was marked persisted")
	}
	j, ok := m.Get(sessionAuditJobID)
	if !ok || !j.TOTPRecoveryPending || !j.TOTPCredentialInvalidated {
		t.Fatal("conflicting persisted identity released quarantine")
	}
	sessionAuditAssertOrdinaryBlocked(t, m, remote)
	before := remote.snapshot()
	if _, err := m.QueueOperation(sessionAuditJobID, "reset-2fa", nil); err == nil {
		t.Fatal("conflicting vault identity was admitted for recovery")
	}
	if after := remote.snapshot(); after != before {
		t.Fatal("conflicting recovery contacted the remote service")
	}
}
