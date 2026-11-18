package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/transport"
)

type operationJournalProtector struct {
	credentials.StateProtector
	beforeWrite func(map[string]any) error
}

func (p *operationJournalProtector) SealContext(ctx context.Context, identity string, raw, aad []byte) ([]byte, error) {
	var out map[string]any
	if json.Unmarshal(raw, &out) == nil && out["reset_totp_requested"] == true && p.beforeWrite != nil {
		if err := p.beforeWrite(out); err != nil {
			return nil, err
		}
	}
	return p.StateProtector.SealContext(ctx, identity, raw, aad)
}
func journalWriteHook(t *testing.T, x *resetFixture, hook func(map[string]any) error) {
	t.Helper()
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	if err := privatestate.Register(filepath.Dir(x.path), &operationJournalProtector{StateProtector: protector, beforeWrite: hook}); err != nil {
		t.Fatal(err)
	}
}

func TestResetJournalContainsOnlyOperationEvidence(t *testing.T) {
	x := newResetFixture(t, false)
	x.flow.Client.Jar.Set(transport.Cookie{Name: "journal-cookie-canary", Value: "MUST-NOT-BE-IN-OPERATION-JOURNAL", Domain: "chat.example", Path: "/"})
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err != nil {
		t.Fatal(err)
	}
	if out["operation_scope"] != "current_session" {
		t.Fatal("missing operation scope")
	}
	stored, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"recovery_schema", "recovery_checkpoint", "preparation_checkpoint", "previous_session_ref", "session_ref", "cookie_state", "cookies", "initial_login_progress"} {
		if _, exists := stored[key]; exists {
			t.Fatalf("obsolete session field %s persisted", key)
		}
	}
	raw, _ := json.Marshal(stored)
	if strings.Contains(string(raw), "MUST-NOT-BE-IN-OPERATION-JOURNAL") {
		t.Fatal("cookie copied into operation evidence")
	}
	if !reflect.DeepEqual(x.calls, []string{"page", "session", "info1", "disable", "enroll", "activate"}) {
		t.Fatalf("unexpected calls %v", x.calls)
	}
}

func TestResetUnknownOperationCannotBeRestartedOrCompletedLocally(t *testing.T) {
	x := newResetFixture(t, false)
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		if step == "disable" {
			return nil, errors.New("synthetic lost response"), true
		}
		return nil, nil, false
	}
	if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil {
		t.Fatal("unknown removal accepted")
	}
	before := len(x.calls)
	if _, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email); err == nil {
		t.Fatal("unknown operation authorized local completion")
	}
	if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
		t.Fatal("unknown operation restarted")
	}
	if countResetSteps(x.calls, "disable") != 1 || countResetSteps(x.calls, "enroll") != 0 || countResetSteps(x.calls, "activate") != 0 {
		t.Fatal("unknown operation replayed mutation")
	}
}

func TestLegacyIncompleteJournalCannotAuthorizeFreshReset(t *testing.T) {
	x := newResetFixture(t, false)
	legacy := map[string]any{"version": 1, "recovery_schema": 1, "reset_totp_requested": true, "email": x.flow.Options.Email, "operation_id": "legacy-operation", "reset_totp_phase": "initial", "activation_succeeded": false, "activation_verified": false, "old_factor_invalidated": false, "credential_persisted": false}
	if err := WritePrivateJSON(x.path, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != 0 {
		t.Fatal("legacy state authenticated or mutated")
	}
}

func TestLegacyResetSessionGuardRejectsWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-checkpoint.json")
	for _, stage := range []string{"totp_reset_recovery", "email_verified"} {
		if err := WriteCheckpoint(path, map[string]any{"stage": stage, "operation_id": "legacy-reset"}); !errors.Is(err, ErrLoginPersistenceRemoved) {
			t.Fatal("legacy session published", err)
		}
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy rejection wrote a checkpoint", err)
	}
}

func TestLegacyCompletionEvidenceRemainsImmutable(t *testing.T) {
	x := newResetFixture(t, false)
	x.path = filepath.Join(filepath.Dir(x.path), "totp-setup-result.json")
	result, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err != nil {
		t.Fatal(err)
	}
	delete(result, "operation_scope")
	result["recovery_schema"] = 1
	result["recovery_checkpoint"] = map[string]any{"cookie_state": map[string]any{"revision": uint64(1<<53) + 7}}
	result["credential_persisted"] = false
	if err := WritePrivateJSON(x.path, result); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(x.path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email)
	if err != nil || !TOTPResetJournalIsReadOnly(read) {
		t.Fatal("legacy activation proof not readable locally", err)
	}
	if _, exists := read["recovery_checkpoint"]; exists {
		t.Fatal("legacy session payload escaped read boundary")
	}
	safeJSON, err := ReadTOTPResetJournalJSON(x.path)
	if err != nil || bytes.Contains(safeJSON, []byte("recovery_checkpoint")) {
		t.Fatal("JSON read API returned old session", err)
	}
	if err := MarkTOTPResetCredentialPersisted(x.path, x.flow.Options.Email, str(result["operation_id"])); err == nil {
		t.Fatal("legacy session evidence was rewritten")
	}
	after, err := os.ReadFile(x.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("legacy original bytes changed", err)
	}
}

func TestResetJournalSingleSessionMarker(t *testing.T) {
	x := newResetFixture(t, false)
	result, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"recovery_checkpoint", "previous_session_ref", "session_ref", "preparation_checkpoint", "reauthentication_previous_checkpoint", "initial_login_progress", "cookie_state", "cookies", "cookie_ref"} {
		t.Run(key, func(t *testing.T) {
			// A mixed-version journal can retain current_session and only one old field.
			path := filepath.Join(t.TempDir(), "totp-setup-result.json")
			const private = "synthetic-session-projection-canary"
			result[key] = private
			defer delete(result, key)
			if err := WritePrivateJSON(path, result); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			read, err := ReadTOTPResetCompletion(path, x.flow.Options.Email)
			if err != nil {
				t.Fatal("lost existing activation evidence", err)
			}
			if _, exposed := read[key]; exposed || !TOTPResetJournalIsReadOnly(read) {
				t.Error("read view must drop the session but retain its read-only origin")
			}
			data, err := ReadTOTPResetJournalJSON(path)
			var roundtrip map[string]any
			if err != nil || bytes.Contains(data, []byte(private)) || json.Unmarshal(data, &roundtrip) != nil || !reflect.DeepEqual(roundtrip, read) {
				t.Fatal("map and JSON readers must return the same session-free view")
			}
			if err := MarkTOTPResetCredentialPersisted(path, x.flow.Options.Email, str(result["operation_id"])); err == nil {
				t.Error("direct mark accepted a read-only record")
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Error("direct mark changed the original bytes")
			}
		})
	}
}

func TestResetPreMutationJournalDoesNotOverrideLegacyProvenance(t *testing.T) {
	x := newResetFixture(t, false)
	prior := map[string]any{
		"version": 1, "operation_scope": "current_session", "email": x.flow.Options.Email, "operation_id": "fixture-op",
		"reset_totp_requested": true, "reset_totp_phase": "initial", "old_factor_invalidated": false, "credential_persisted": false,
		"mutation_schema": 1, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
		"activation_succeeded": false, "activation_verified": false, "activation_attempted": false, "enroll_attempted": false,
	}
	if err := WritePrivateJSON(x.path, prior); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTOTPResetStart(x.path, x.flow.Options.Email); err != nil {
		t.Fatal("ordinary clean pre-mutation record should allow a fresh explicit reset", err)
	}
	prior["recovery_checkpoint"] = nil // Presence alone matters, not a truthy payload.
	if err := WritePrivateJSON(x.path, prior); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(x.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != 0 {
		t.Error("session-bearing initial record admitted fresh network work")
	}
	if after, err := os.ReadFile(x.path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("preflight changed the legacy evidence")
	}
}
