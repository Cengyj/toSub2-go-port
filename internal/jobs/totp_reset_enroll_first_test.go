package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func enrollFirstBeginJournal(j *Job, state string) map[string]any {
	return map[string]any{
		"version": 1, "operation_scope": "current_session", "email": j.Email,
		"operation_id": "enroll-first-operation", "account_id": "fixture-account",
		"reset_totp_requested": true, "reset_totp_phase": "quarantine_intent",
		"reset_totp_strategy": "enroll_first", "initial_totp_state": state,
		"mutation_schema": 1, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent",
		"activation_dispatch": "not_sent", "initial_enroll_dispatch": "not_sent",
		"enroll_attempted": false, "activation_attempted": false, "initial_enroll_attempted": false,
		"old_factor_invalidated": false, "activation_succeeded": false,
		"activation_verified": false, "credential_persisted": false,
	}
}

func writeEnrollFirstBeginJournal(t *testing.T, m *Manager, j *Job, out map[string]any) {
	t.Helper()
	registerCompletionFixture(t, m.store.Root)
	if err := protocol.WritePrivateJSON(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json"), out); err != nil {
		t.Fatal(err)
	}
}

func TestBeginTOTPResetEnrollFirstQuarantinesWithoutOldFactorAndSurvivesRestart(t *testing.T) {
	for _, state := range []string{"disabled", "unknown"} {
		t.Run(state, func(t *testing.T) {
			m, vault, j, peer := resetManagerFixture(t)
			if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			journal := enrollFirstBeginJournal(j, state)
			writeEnrollFirstBeginJournal(t, m, j, journal)
			if err := m.BeginTOTPReset(t.Context(), j.ID, "enroll-first-operation"); err != nil {
				t.Fatal("verified enrollment-first intent requires no old factor", err)
			}
			stored, err := vault.Load(j.Email)
			if err != nil || !stored.TOTPResetPending || stored.TOTPResetOperationID != "enroll-first-operation" || stored.TOTPSecret != "OLDSEEDCANARY" {
				t.Fatal("pending binding was not saved while preserving the previous credential")
			}
			active, _ := m.Get(j.ID)
			if active.TOTPResetStatus != "preparing" || active.TOTPResetPhase != "quarantine_committed" {
				t.Fatalf("enrollment-first intent was incorrectly recorded as a removal: status=%s phase=%s", active.TOTPResetStatus, active.TOTPResetPhase)
			}
			for _, id := range []string{j.ID, peer.ID} {
				got, _ := m.Get(id)
				if !got.TOTPRecoveryPending || !got.TOTPCredentialInvalidated || got.HasTOTPKey || got.Input["totpSecret"] != nil || got.TOTPResetOperationID != "enroll-first-operation" {
					t.Fatalf("account row %s was not quarantined before enrollment", id)
				}
			}
			metadata, err := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
			if err != nil || !strings.Contains(string(metadata), "enroll-first-operation") || strings.Contains(string(metadata), "OLDSEEDCANARY") {
				t.Fatal("durable metadata is missing the operation or leaked the old seed")
			}
			restored := NewManager(context.Background(), 1, m.store, nil)
			restored.SetCredentialStore(vault)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{j.ID, peer.ID} {
				got, _ := restored.Get(id)
				if !got.TOTPRecoveryPending || !got.TOTPCredentialInvalidated || got.HasTOTPKey {
					t.Fatalf("restart released pending enrollment quarantine for %s", id)
				}
				if _, err := restored.LoadCredentials(id); !errors.Is(err, ErrTOTPRecoveryRequired) {
					t.Fatalf("restart exposed a quarantined credential: %v", err)
				}
			}
			if len(restored.pending) != 0 {
				t.Fatal("restart scheduled another remote attempt")
			}
		})
	}
}

func TestBeginTOTPResetEnrollFirstRejectsSentOrInvalidEvidenceWithoutVaultChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing strategy", func(out map[string]any) { delete(out, "reset_totp_strategy") }},
		{"unsupported strategy", func(out map[string]any) { out["reset_totp_strategy"] = "try_anything" }},
		{"missing initial state", func(out map[string]any) { delete(out, "initial_totp_state") }},
		{"already enabled without factor", func(out map[string]any) { out["initial_totp_state"] = "enabled" }},
		{"missing enrollment dispatch", func(out map[string]any) { delete(out, "initial_enroll_dispatch") }},
		{"invalid enrollment dispatch", func(out map[string]any) { out["initial_enroll_dispatch"] = "unknown" }},
		{"missing attempted flag", func(out map[string]any) { delete(out, "initial_enroll_attempted") }},
		{"sent initial enrollment", func(out map[string]any) {
			out["initial_enroll_attempted"], out["initial_enroll_dispatch"] = true, "may_have_been_sent"
			out["initial_enroll_attempt_id"], out["old_factor_invalidated"] = "sent-attempt", true
		}},
		{"earlier enrollment was proven unsent", func(out map[string]any) {
			out["initial_enroll_attempted"], out["initial_enroll_dispatch"], out["old_factor_invalidated"] = true, "proven_unsent", true
		}},
		{"unexpected new material", func(out map[string]any) { out["secret"] = completionFixtureSeed }},
		{"wrong account", func(out map[string]any) { out["email"] = "another@example.test" }},
		{"missing account binding", func(out map[string]any) { delete(out, "account_id") }},
		{"wrong operation", func(out map[string]any) { out["operation_id"] = "different-operation" }},
		{"wrong phase", func(out map[string]any) { out["reset_totp_phase"] = "initial" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, vault, j, peer := resetManagerFixture(t)
			if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			before, err := vault.Load(j.Email)
			if err != nil {
				t.Fatal(err)
			}
			journal := enrollFirstBeginJournal(j, "unknown")
			tc.edit(journal)
			writeEnrollFirstBeginJournal(t, m, j, journal)
			if err := m.BeginTOTPReset(t.Context(), j.ID, "enroll-first-operation"); err == nil {
				t.Fatal("invalid or previously attempted enrollment intent was acknowledged")
			}
			after, err := vault.Load(j.Email)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("rejected intent changed saved credentials")
			}
			for _, id := range []string{j.ID, peer.ID} {
				got, _ := m.Get(id)
				if got.TOTPRecoveryPending || got.TOTPCredentialInvalidated || got.TOTPResetOperationID != "" {
					t.Fatalf("rejected intent unexpectedly quarantined row %s", id)
				}
			}
		})
	}
}
