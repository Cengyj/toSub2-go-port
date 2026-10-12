package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/store"
)

func initialResetJournal(email string) map[string]any {
	return map[string]any{
		"version": 1, "email": email, "reset_totp_requested": true,
		"reset_totp_phase": "initial", "reset_totp_status": "failed",
		"activation_succeeded": false, "activation_verified": false,
		"old_factor_invalidated": false, "credential_persisted": false,
		"reset_totp_attempted_at": nil, "logout_all_devices_attempted_at": nil,
	}
}

func TestOtherTOTPJournalOnlyPermitsStrictlyUnmodifiedInitialState(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want bool
	}{
		{"initial", func(map[string]any) {}, true},
		{"missing-proof", func(j map[string]any) { delete(j, "old_factor_invalidated") }, false},
		{"attempted", func(j map[string]any) { j["reset_totp_attempted_at"] = "2026-10-12T00:00:00Z" }, false},
		{"old-factor-read", func(j map[string]any) { j["old_totp_factor_id"] = "factor" }, false},
		{"new-secret", func(j map[string]any) { j["secret"] = "new-seed" }, false},
		{"activated", func(j map[string]any) { j["activation_succeeded"] = true }, false},
		{"disabled", func(j map[string]any) { j["reset_totp_phase"] = "disabled_verified" }, false},
		{"wrong-email", func(j map[string]any) { j["email"] = "other@example.test" }, false},
		{"unknown-version", func(j map[string]any) { j["version"] = 2 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, j, peer := resetManagerFixture(t)
			private, err := store.New(filepath.Join(m.store.Root, peer.ID))
			if err != nil {
				t.Fatal(err)
			}
			journal := initialResetJournal(j.Email)
			tc.edit(journal)
			if err := private.WriteJSON(context.Background(), "totp-setup-result.json", journal); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(private.Path("totp-setup-result.json"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.QueueOperation(j.ID, "reset-2fa", nil)
			if tc.want && err != nil || !tc.want && !errors.Is(err, ErrTOTPRecoveryRequired) {
				t.Fatalf("initial journal eligibility=%v, error=%v", tc.want, err)
			}
			after, readErr := os.ReadFile(private.Path("totp-setup-result.json"))
			if readErr != nil || string(before) != string(after) {
				t.Fatal("eligibility changed previous private journal")
			}
		})
	}
}

func TestOtherInitialTOTPJournalAllowsFreshResetAfterRestart(t *testing.T) {
	m, vault, j, peer := resetManagerFixture(t)
	private, err := store.New(filepath.Join(m.store.Root, peer.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := private.WriteJSON(context.Background(), "totp-setup-result.json", initialResetJournal(j.Email)); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(context.Background(), 1, m.store, nil)
	restored.SetCredentialStore(vault)
	if err := restored.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal("strictly pre-mutation other-row journal locked an unmodified account after restart", err)
	}
	if err := restored.BeginTOTPReset(context.Background(), j.ID); err != nil {
		t.Fatal("strictly pre-mutation other-row journal blocked removal acknowledgement", err)
	}
	if _, err := os.Stat(private.Path("totp-setup-result.json")); err != nil {
		t.Fatal("safe historical journal was deleted")
	}
}

func TestOtherMalformedTOTPJournalNeverUnlocksAccount(t *testing.T) {
	for _, kind := range []string{"malformed", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			m, _, j, peer := resetManagerFixture(t)
			root := filepath.Join(m.store.Root, peer.ID)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "totp-setup-result.json")
			var err error
			switch kind {
			case "malformed":
				err = os.WriteFile(path, []byte(`{"version":1`), 0o600)
			case "symlink":
				err = os.Symlink(filepath.Join(root, "missing-target"), path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); !errors.Is(err, ErrTOTPRecoveryRequired) {
				t.Fatal("unsafe private evidence became retryable", err)
			}
		})
	}
}
