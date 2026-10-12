package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func resetManagerFixture(t *testing.T) (*Manager, *memoryVault, *Job, *Job) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v := &memoryVault{}
	m := NewManager(context.Background(), 1, st, nil)
	m.SetCredentialStore(v)
	j, err := m.Submit(Spec{ID: "reset", Email: "reset@example.test", ImportOnly: true, Input: map[string]any{"password": "fixture-password", "totpSecret": "OLDSEEDCANARY"}})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := m.Submit(Spec{ID: "older", Email: j.Email, ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return m, v, j, peer
}

func TestTOTPResetQuarantineBlocksEveryAccountEntryPointAndSurvivesRestart(t *testing.T) {
	m, v, j, peer := resetManagerFixture(t)
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LoadCredentials(peer.ID); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal("queued same-email reset did not block export")
	}
	if err := m.BeginTOTPReset(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	c, _ := v.Load(j.Email)
	if !c.TOTPResetPending || c.TOTPSecret != "OLDSEEDCANARY" {
		t.Fatal("encrypted recovery seed was not quarantined")
	}
	for _, id := range []string{j.ID, peer.ID} {
		got, _ := m.Get(id)
		if !got.TOTPRecoveryPending || !got.TOTPCredentialInvalidated || got.HasTOTPKey || got.Input["totpSecret"] != nil {
			t.Fatalf("unsafe job %s", id)
		}
		for name, action := range map[string]func() error{
			"retry":   func() error { _, e := m.Retry(id); return e },
			"queue":   func() error { _, e := m.QueueOperation(id, "relogin", nil); return e },
			"requeue": func() error { _, e := m.Requeue(id, "full"); return e },
			"reset":   func() error { _, e := m.QueueOperation(id, "reset-2fa", nil); return e },
			"update":  func() error { _, e := m.Update(id, map[string]any{"totpSecret": "stale"}); return e },
			"repair":  func() error { _, e := m.QueueAutoRepair(id, []int{1}, "backend"); return e },
			"export":  func() error { _, e := m.LoadCredentials(id); return e },
		} {
			if err := action(); !errors.Is(err, ErrTOTPRecoveryRequired) {
				t.Fatalf("%s %s allowed: %v", id, name, err)
			}
		}
		if m.Input(id, map[string]any{"totpSecret": "stale"}) {
			t.Fatal("input requeued a quarantined job")
		}
	}
	if _, err := m.Submit(Spec{Email: j.Email, Input: map[string]any{"totpSecret": "stale"}}); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
	if strings.Contains(string(data), "OLDSEEDCANARY") || strings.Contains(string(data), "fixture-password") {
		t.Fatal("secret in public metadata")
	}
	restarted := NewManager(context.Background(), 1, m.store, nil)
	restarted.SetCredentialStore(v)
	if err := restarted.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		got, _ := restarted.Get(id)
		if got.Status != StatusFailed || !got.TOTPRecoveryPending || got.HasTOTPKey {
			t.Fatalf("unsafe restart: %#v", got)
		}
	}
	if len(restarted.pending) != 0 {
		t.Fatal("reset replayed after restart")
	}
}

func TestTOTPResetCommitRequiresReadbackAndReplacesPeerSecrets(t *testing.T) {
	m, v, j, peer := resetManagerFixture(t)
	_, _ = m.QueueOperation(j.ID, "reset-2fa", nil)
	if err := m.BeginTOTPReset(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	c, _ := v.Load(j.Email)
	c.TOTPSecret, c.TOTPResetPending = "NEWSEEDCANARY", false
	if err := m.CommitTOTPReset(context.Background(), j.ID, c); err == nil {
		t.Fatal("accepted unpersisted new secret")
	}
	if err := v.Save(j.Email, c); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitTOTPReset(context.Background(), j.ID, c); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		got, _ := m.Get(id)
		if got.TOTPRecoveryPending || got.TOTPCredentialInvalidated || !got.HasTOTPKey || got.Input["totpSecret"] != c.TOTPSecret {
			t.Fatal("commit did not update same-email secret")
		}
	}
	// A stale result snapshot cannot undo a completed durable commit.
	if err := m.UpdateTOTPResetResult(j.ID, map[string]any{"reset_totp_requested": true, "old_factor_invalidated": true, "reset_totp_phase": "activation_verified"}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(j.ID)
	if got.TOTPResetPhase != "credential_persisted" || got.TOTPRecoveryPending {
		t.Fatal("stale result undid commit")
	}
}

func TestTOTPResetRejectsMemoryOnlyPersistence(t *testing.T) {
	m, _, j, _ := resetManagerFixture(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	_, _ = m.QueueOperation(j.ID, "reset-2fa", nil)
	if err := m.BeginTOTPReset(context.Background(), j.ID); err == nil {
		t.Fatal("memory store acknowledged destructive reset")
	}
}

type mismatchingResetVault struct {
	*memoryVault
	corrupt bool
}

func (v *mismatchingResetVault) Load(email string) (credentials.Credentials, error) {
	c, e := v.memoryVault.Load(email)
	if v.corrupt && c.TOTPResetPending {
		c.TOTPResetPending = false
	}
	return c, e
}

func TestTOTPResetReadbackFailureRetainsQuarantine(t *testing.T) {
	m, v, j, _ := resetManagerFixture(t)
	m.SetCredentialStore(&mismatchingResetVault{memoryVault: v, corrupt: true})
	_, _ = m.QueueOperation(j.ID, "reset-2fa", nil)
	if err := m.BeginTOTPReset(context.Background(), j.ID); err == nil {
		t.Fatal("mismatched readback acknowledged")
	}
	got, _ := m.Get(j.ID)
	if !got.TOTPRecoveryPending {
		t.Fatal("uncertain write released quarantine")
	}
}

func TestTOTPResetRestoreNeverReplaysAndRetainsUnverifiedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                string
		activated, verified bool
	}{
		{"disable_intent", false, false}, {"activation_unverified", true, false}, {"activation_verified", true, true}, {"recovery_required", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, v, j, peer := resetManagerFixture(t)
			_, _ = m.QueueOperation(j.ID, "reset-2fa", map[string]any{"logoutAllDevicesAfterTotp": true})
			if err := m.BeginTOTPReset(context.Background(), j.ID); err != nil {
				t.Fatal(err)
			}
			result := map[string]any{"version": 1, "email": j.Email, "reset_totp_requested": true, "old_factor_invalidated": true, "reset_totp_status": "unknown", "reset_totp_phase": tc.name, "logout_all_devices_requested": true, "logout_all_devices_status": "pending"}
			if tc.activated {
				result["activation_succeeded"] = true
				result["secret"] = "NEWSEEDCANARY"
			}
			if tc.verified {
				result["activation_verified"] = true
				result["reset_totp_status"] = "activated"
				if tc.name == "recovery_required" {
					result["reset_totp_status"] = "unknown"
				}
			}
			privateStore, err := store.New(filepath.Join(m.store.Root, j.ID))
			if err != nil {
				t.Fatal(err)
			}
			if err := privateStore.WriteJSON(context.Background(), "totp-setup-result.json", result); err != nil {
				t.Fatal(err)
			}
			restarted := NewManager(context.Background(), 1, m.store, nil)
			restarted.SetCredentialStore(v)
			var runs atomic.Int32
			restarted.SetRunner(func(context.Context, *Job, func(JobEvent)) error { runs.Add(1); return nil })
			if err := restarted.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, _ := restarted.Get(j.ID)
			related, _ := restarted.Get(peer.ID)
			c, _ := v.Load(j.Email)
			if tc.verified {
				if got.TOTPRecoveryPending || related.TOTPRecoveryPending || c.TOTPResetPending || got.TOTPResetPhase != "credential_persisted" {
					t.Fatal("verified recovery stayed quarantined")
				}
				if _, e := os.Stat(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")); !os.IsNotExist(e) {
					t.Fatal("committed private evidence was not cleaned")
				}
			} else {
				if !got.TOTPRecoveryPending || !related.TOTPRecoveryPending || !c.TOTPResetPending || got.Status != StatusFailed {
					t.Fatal("uncertain recovery released quarantine")
				}
				if _, e := os.Stat(filepath.Join(m.store.Root, j.ID, "totp-setup-result.json")); e != nil {
					t.Fatal("uncertain recovery evidence removed")
				}
			}
			if tc.activated && c.TOTPSecret != "NEWSEEDCANARY" {
				t.Fatal("activated new secret was not protected")
			}
			if got.LogoutAllDevicesStatus != "not_attempted" {
				t.Fatal("restore may replay logout")
			}
			restarted.Start()
			closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := restarted.Shutdown(closeCtx); err != nil {
				t.Fatal(err)
			}
			if runs.Load() != 0 {
				t.Fatal("restore replayed remote operation")
			}
		})
	}
}

func TestTOTPResetResultMetadataRedactsSecrets(t *testing.T) {
	m, _, j, _ := resetManagerFixture(t)
	_, _ = m.QueueOperation(j.ID, "reset-2fa", nil)
	result := map[string]any{"reset_totp_requested": true, "reset_totp_status": "failed", "secret": "NEWSEEDCANARY", "otpauth_uri": "otpauth://totp/secret-canary", "reset_totp_error": "failed NEWSEEDCANARY", "logout_all_devices_error": "failed otpauth://totp/secret-canary"}
	if err := m.UpdateTOTPResetResult(j.ID, result, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(j.ID)
	b, _ := json.Marshal(SanitizeJobForPublic(got))
	disk, _ := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
	for _, data := range [][]byte{b, disk} {
		if strings.Contains(string(data), "NEWSEEDCANARY") || strings.Contains(string(data), "secret-canary") {
			t.Fatal("private reset material leaked")
		}
	}
}

func TestTOTPResetPreflightRestartDoesNotReplayOrQuarantine(t *testing.T) {
	m, v, j, _ := resetManagerFixture(t)
	m.mu.Lock()
	m.jobs[j.ID].Status = StatusSucceeded
	m.mu.Unlock()
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(context.Background(), 1, m.store, nil)
	restarted.SetCredentialStore(v)
	if err := restarted.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := restarted.Get(j.ID)
	if got.Status != StatusFailed || got.TOTPRecoveryPending || len(restarted.pending) != 0 {
		t.Fatal("preflight reset replayed or unnecessarily quarantined")
	}
	if _, err := restarted.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatalf("safe preflight cannot retry: %v", err)
	}
}

func TestTOTPResetOrphanIntentWithoutVaultStillBlocks(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private, err := store.New(filepath.Join(st.Root, "orphan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := private.WriteJSON(context.Background(), "totp-setup-result.json", map[string]any{"version": 1, "email": "orphan@example.test", "reset_totp_requested": true, "old_factor_invalidated": true, "reset_totp_phase": "disable_intent"}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), 1, st, nil)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, ok := m.Get("orphan")
	if !ok || !j.TOTPRecoveryPending || j.Status != StatusFailed {
		t.Fatal("orphan disable intent was not quarantined")
	}
	if _, err := m.Retry(j.ID); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal("orphan intent was retryable")
	}
}

type delayedResetExportVault struct {
	*memoryVault
	first            atomic.Bool
	started, release chan struct{}
}

func (v *delayedResetExportVault) Load(email string) (credentials.Credentials, error) {
	c, err := v.memoryVault.Load(email)
	if v.first.CompareAndSwap(false, true) {
		close(v.started)
		<-v.release
	}
	return c, err
}

func TestTOTPResetConcurrentExportRechecksStateAfterVaultRead(t *testing.T) {
	m, v, j, _ := resetManagerFixture(t)
	delayed := &delayedResetExportVault{memoryVault: v, started: make(chan struct{}), release: make(chan struct{})}
	m.SetCredentialStore(delayed)
	result := make(chan error, 1)
	go func() { _, err := m.LoadCredentials(j.ID); result <- err }()
	<-delayed.started
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginTOTPReset(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	close(delayed.release)
	if err := <-result; !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatalf("export returned a pre-reset secret: %v", err)
	}
}

func TestTOTPResetOtherJobJournalBlocksNewFactorReplacement(t *testing.T) {
	m, _, j, peer := resetManagerFixture(t)
	private, err := store.New(filepath.Join(m.store.Root, peer.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := private.WriteJSON(context.Background(), "totp-setup-result.json", map[string]any{"version": 1, "email": j.Email, "secret": "OLDER_VERIFIED_SEED", "activation_succeeded": true, "activation_verified": true, "reset_totp_requested": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); !errors.Is(err, ErrTOTPRecoveryRequired) {
		t.Fatal("older same-email journal allowed a new reset")
	}
}

func TestTOTPResetRestoreDoesNotReplaceNewerCommittedSeed(t *testing.T) {
	m, v, j, _ := resetManagerFixture(t)
	private, err := store.New(filepath.Join(m.store.Root, j.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := private.WriteJSON(context.Background(), "totp-setup-result.json", map[string]any{"version": 1, "email": j.Email, "secret": "OLD_JOURNAL_SEED", "activation_succeeded": true, "activation_verified": true, "old_factor_invalidated": true, "reset_totp_requested": true, "reset_totp_status": "activated"}); err != nil {
		t.Fatal(err)
	}
	c, _ := v.Load(j.Email)
	c.TOTPSecret = "LATEST_SAVED_SEED"
	c.TOTPResetPending = false
	_ = v.Save(j.Email, c)
	restarted := NewManager(context.Background(), 1, m.store, nil)
	restarted.SetCredentialStore(v)
	if err := restarted.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := v.Load(j.Email)
	got, _ := restarted.Get(j.ID)
	if current.TOTPSecret != "LATEST_SAVED_SEED" || !got.TOTPRecoveryPending {
		t.Fatal("historical recovery overwrote the current committed seed")
	}
	if _, err := os.Stat(private.Path("totp-setup-result.json")); err != nil {
		t.Fatal("conflicting evidence deleted")
	}
}

func TestTOTPResetCompletionDoesNotOfferInvalidatedCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name, status, want string
		output             bool
	}{
		{"imported", StatusImported, StatusImported, false},
		{"authorized", StatusSucceeded, StatusSucceeded, true},
		{"checkpoint", "resume_available", "reauth_required", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, v, j, _ := resetManagerFixture(t)
			m.mu.Lock()
			m.jobs[j.ID].Status = tc.status
			m.jobs[j.ID].ResultSaved = tc.output
			m.mu.Unlock()
			m.SetRunner(func(ctx context.Context, job *Job, _ func(JobEvent)) error {
				if err := m.BeginTOTPReset(ctx, job.ID); err != nil {
					return err
				}
				c, err := v.Load(job.Email)
				if err != nil {
					return err
				}
				c.TOTPSecret = "NEWSEEDCANARY"
				c.TOTPResetPending = false
				if err := v.Save(job.Email, c); err != nil {
					return err
				}
				return m.CommitTOTPReset(ctx, job.ID, c)
			})
			if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			m.Start()
			defer m.Shutdown(context.Background())
			got := waitForStatus(t, m, j.ID, tc.want)
			if got.TOTPResetPhase != "credential_persisted" || got.ResultSaved != tc.output || got.TOTPRecoveryPending {
				t.Fatal("reset completion lost durable state")
			}
			if tc.name == "checkpoint" && !strings.Contains(got.Prompt, "重新登录") {
				t.Fatal("invalidated session still presented as resumable")
			}
		})
	}
}
