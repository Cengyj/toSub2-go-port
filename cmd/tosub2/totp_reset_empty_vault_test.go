package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

type resetEmptyVault struct {
	*credentials.MemoryStore
	failBaseline   bool
	dropBaseline   bool
	baselineWrites atomic.Int32
}

func (v *resetEmptyVault) Save(email string, value credentials.Credentials) error {
	if !value.TOTPResetPending && value.TOTPSecret == "" && value.PendingTOTPSecret == "" && value.Password == "" {
		v.baselineWrites.Add(1)
		if v.failBaseline {
			return errors.New("synthetic baseline persistence failure")
		}
		if v.dropBaseline {
			return nil // An acknowledged save is not sufficient without readback.
		}
	}
	return v.MemoryStore.Save(email, value)
}

func TestTOTPResetSavedSessionInitializesMissingVaultBindingBeforeNetwork(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, scenario := range []struct {
		name       string
		fail, drop bool
	}{{name: "missing-record"}, {name: "baseline-write-failed", fail: true}, {name: "baseline-readback-missing", drop: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
			vault := &resetEmptyVault{MemoryStore: credentials.NewMemoryStore(), failBaseline: scenario.fail, dropBaseline: scenario.drop}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := vault.Load(sessionAuditEmail); err != nil {
					t.Error("reset contacted the server before preserving its recovery binding")
				}
				remote.serveHTTP(w, r)
			}))
			defer server.Close()
			cfg := sessionAuditConfig(t.TempDir(), server.URL)
			m, _, logger := sessionAuditManager(t, cfg, vault)
			job, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.UpdateTOTPSetup(job.ID, "", "", "", true); err != nil {
				t.Fatal(err)
			}
			job, err = m.QueueOperation(job.ID, "reset-2fa", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := vault.Load(job.Email); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fixture unexpectedly retained an old vault entry", err)
			}
			t.Setenv("CHATGPT_LOGIN_PASSWORD", "synthetic-password")
			err = sessionAuditExecute(t, cfg, m, logger, vault)
			if vault.baselineWrites.Load() != 1 {
				t.Fatal("missing recovery binding was not saved exactly once")
			}
			if scenario.fail || scenario.drop {
				if err == nil || remote.snapshot().requests != 0 {
					t.Fatal("failed initial binding crossed the network barrier", err)
				}
				return
			}
			if err != nil {
				t.Fatal("valid original session could not reset an account without an old vault record", err)
			}
			state := remote.snapshot()
			if state.disableCalls != 1 || state.enrollCalls != 1 || state.activateCalls != 1 || state.loginRequests != 1 || state.unauthenticated != 0 {
				t.Fatal("empty local baseline changed account proof or repeated a mutation", state)
			}
			sessionAuditAssertCommitted(t, cfg, m, vault, remote)
		})
	}
}
