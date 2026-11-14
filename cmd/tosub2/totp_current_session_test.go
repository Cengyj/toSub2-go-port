package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestTOTPCurrentSessionUnknownMutationReadsWithoutReplay(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, fault := range []string{"disable-response-lost", "activation-response-lost", "enroll-response-lost", "disable-message-changed"} {
		t.Run(fault, func(t *testing.T) {
			remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor", failure: fault}
			server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
			defer server.Close()
			cfg := sessionAuditConfig(t.TempDir(), server.URL)
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			m, _, logger := sessionAuditManager(t, cfg, vault)
			sessionAuditPrepare(t, cfg, m)
			err := sessionAuditExecute(t, cfg, m, logger, vault)
			state := remote.snapshot()
			wantReads, wantActivations := 2, 1
			if fault == "disable-message-changed" {
				wantReads = 1
			}
			if fault == "enroll-response-lost" {
				wantReads, wantActivations = 1, 0
			}
			if state.disableCalls != 1 || state.enrollCalls != 1 || state.activateCalls != wantActivations || state.infoReads != wantReads {
				t.Fatalf("unknown result repeated mutation or performed an unnecessary read: %+v", state)
			}
			if fault == "enroll-response-lost" {
				if err == nil {
					t.Fatal("lost enrollment material reported complete")
				}
				if _, err := m.QueueOperation(sessionAuditJobID, "complete-2fa", nil); err == nil {
					t.Fatal("missing enrollment material admitted for local completion")
				}
				sessionAuditAssertOrdinaryBlocked(t, m, remote)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sessionAuditAssertCommitted(t, cfg, m, vault, remote)
			evidence := sessionAuditJournal(t, cfg)
			wantSource := "business_receipt"
			if fault == "activation-response-lost" {
				wantSource = "state_read"
			}
			if evidence["activation_confirmation_source"] != wantSource {
				t.Fatal("confirmation source changed or was fabricated")
			}
		})
	}
}
