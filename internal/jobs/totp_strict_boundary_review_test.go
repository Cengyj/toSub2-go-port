package jobs

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestStrictTOTPStaleMetadataCannotHideRetainedCompletionEvidence(t *testing.T) {
	writer, vault, owner, peer := resetManagerFixture(t)
	reader := NewManager(t.Context(), 1, writer.store, nil)
	reader.SetCredentialStore(vault)
	if err := reader.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.QueueOperation(owner.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := beginTOTPResetFixture(t, writer, t.Context(), owner.ID, "fixture-op"); err != nil {
		t.Fatal(err)
	}
	path := uncertainResetJournal(t, writer, owner)
	writeCompletionFixture(t, path)
	committed, err := vault.Load(owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	committed.TOTPSecret, committed.TOTPResetPending = completionFixtureSeed, false
	if err := vault.Save(owner.Email, committed); err != nil {
		t.Fatal(err)
	}
	if err := commitTOTPResetFixture(t, writer, t.Context(), owner.ID, committed); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := reader.Get(owner.ID)
	if stale.ResetTOTP || stale.TOTPRecoveryPending {
		t.Fatal("fixture is not an older in-memory metadata snapshot")
	}
	for _, id := range []string{owner.ID, peer.ID} {
		if err := reader.CheckTOTPExport(id); !errors.Is(err, ErrTOTPRecoveryRequired) {
			t.Errorf("stale metadata hid retained completion journal from export gate: job=%s err=%v", id, err)
		}
		if _, err := reader.LoadCredentials(id); !errors.Is(err, ErrTOTPRecoveryRequired) {
			t.Errorf("stale metadata released credentials before local handoff: job=%s err=%v", id, err)
		}
	}
	after, readErr := os.ReadFile(path)
	afterCredential, loadErr := vault.Load(owner.Email)
	if readErr != nil || loadErr != nil || string(before) != string(after) || !reflect.DeepEqual(committed, afterCredential) {
		t.Fatal("read-only checks modified recovery evidence", readErr, loadErr)
	}
}

type strictReviewUnavailableVault struct{ credentials.Store }

func (v *strictReviewUnavailableVault) Load(string) (credentials.Credentials, error) {
	return credentials.Credentials{}, errors.New("synthetic private vault unavailable: /fixture/provider-path/private-secret-canary")
}

func TestStrictTOTPExportGateCannotConfirmSafetyWhenConfiguredVaultIsUnavailable(t *testing.T) {
	m, vault, owner, peer := resetManagerFixture(t)
	private, err := vault.Load(owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	private.TOTPResetPending = true
	if err := vault.Save(owner.Email, private); err != nil {
		t.Fatal(err)
	}
	m.SetCredentialStore(&strictReviewUnavailableVault{Store: vault})
	for _, id := range []string{owner.ID, peer.ID} {
		if err := m.CheckTOTPExport(id); !errors.Is(err, ErrTOTPExportUnavailable) || strings.Contains(err.Error(), "private-secret-canary") || strings.Contains(err.Error(), "provider-path") {
			t.Errorf("unreadable configured vault lacked a safe rejection: job=%s err=%v", id, err)
		}
	}
	if err := m.CheckTOTPExportEmail(owner.Email); err == nil {
		t.Error("raw payload gate ignored unavailable private recovery state")
	}
	after, err := vault.Load(owner.Email)
	if err != nil || !reflect.DeepEqual(private, after) {
		t.Fatal("export safety check changed private credentials", err)
	}
}

type strictReviewCountingVault struct {
	credentials.Store
	loads int
}

func (v *strictReviewCountingVault) Load(email string) (credentials.Credentials, error) {
	v.loads++
	return v.Store.Load(email)
}

func TestStrictTOTPExportGateReadsHealthyVaultOnceAndAcceptsUnknownExternalAccount(t *testing.T) {
	m, vault, owner, _ := resetManagerFixture(t)
	counted := &strictReviewCountingVault{Store: vault}
	m.SetCredentialStore(counted)
	if m.TOTPExportPending(owner.ID) || counted.loads != 0 {
		t.Fatal("local public projection consulted credentials")
	}
	if err := m.CheckTOTPExport(owner.ID); err != nil || counted.loads != 1 {
		t.Fatal("ordinary export did not use one healthy vault read", err, counted.loads)
	}
	counted.loads = 0
	if err := m.CheckTOTPExportEmail("unknown-external@example.test"); err != nil || counted.loads != 1 {
		t.Fatal("unknown external account required local credentials", err, counted.loads)
	}
}
