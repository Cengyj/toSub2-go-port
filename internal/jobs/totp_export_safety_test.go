package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

func TestTOTPExportSharedVaultSafety(t *testing.T) {
	for _, entry := range []string{"job", "email", "workflow"} {
		for _, scenario := range []string{"nil", "unreadable", "missing", "pending-secret", "pending-uri", "pending-factor"} {
			t.Run(entry+"/"+scenario, func(t *testing.T) {
				m, vault, owner, _ := resetManagerFixture(t)
				want := ErrTOTPSetupVerificationPending
				saved, err := vault.Load(owner.Email)
				if err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "nil":
					m.SetCredentialStore(nil)
					want = ErrTOTPExportUnavailable
				case "unreadable":
					m.SetCredentialStore(&strictReviewUnavailableVault{Store: vault})
					want = ErrTOTPExportUnavailable
				case "missing":
					if err := vault.Delete(owner.Email); err != nil {
						t.Fatal(err)
					}
					want = nil
				case "pending-secret":
					saved.PendingTOTPSecret = "JBSWY3DPEHPK3PXP"
				case "pending-uri":
					saved.PendingTOTPURI = "otpauth://totp/synthetic?secret=JBSWY3DPEHPK3PXP"
				case "pending-factor":
					saved.PendingTOTPFactorID = "synthetic-unverified-factor"
				}
				if scenario != "missing" {
					if err := vault.Save(owner.Email, saved); err != nil {
						t.Fatal(err)
					}
				}
				switch entry {
				case "job":
					err = m.CheckTOTPExport(owner.ID)
				case "email":
					err = m.CheckTOTPExportEmail(owner.Email)
				case "workflow":
					err = m.CheckTOTPWorkflowExport(owner.ID)
				}
				if !errors.Is(err, want) {
					t.Fatalf("export gate accepted unsafe shared state or rejected a definite absence: got=%v want=%v", err, want)
				}
			})
		}
	}
}

func TestTOTPExportEmptyIDRemainsMissing(t *testing.T) {
	m, _, _, _ := resetManagerFixture(t)
	for _, entry := range []struct {
		name string
		call func() error
	}{
		{"job", func() error { return m.CheckTOTPExport("") }},
		{"job-context", func() error { return m.CheckTOTPExportContext(t.Context(), "") }},
		{"workflow", func() error { return m.CheckTOTPWorkflowExport("") }},
		{"workflow-context", func() error { return m.CheckTOTPWorkflowExportContext(t.Context(), "") }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			if err := entry.call(); err == nil || err.Error() != osErrNotFound("").Error() {
				t.Fatal("empty job ID was treated as an external email", err)
			}
		})
	}
}

type exportCapturedVault struct {
	credentials.Store
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (v *exportCapturedVault) finish() { v.once.Do(func() { close(v.release) }) }
func (v *exportCapturedVault) Load(email string) (credentials.Credentials, error) {
	return v.LoadContext(context.Background(), email)
}
func (v *exportCapturedVault) LoadContext(ctx context.Context, email string) (credentials.Credentials, error) {
	value, err := v.Store.Load(email)
	if v.calls.Add(1) == 1 {
		close(v.entered)
		select {
		case <-v.release:
		case <-ctx.Done():
			return credentials.Credentials{}, errors.Join(ctx.Err(), errors.New("synthetic-private-vault-diagnostic"))
		}
	}
	return value, err
}
func (v *exportCapturedVault) SaveContext(ctx context.Context, email string, c credentials.Credentials) error {
	return credentials.SaveContext(ctx, v.Store, email, c)
}
func (v *exportCapturedVault) DeleteContext(ctx context.Context, email string) error {
	return credentials.DeleteContext(ctx, v.Store, email)
}

func TestTOTPExportVaultReadDoesNotHoldManagerAndRejectsReplacement(t *testing.T) {
	m, vault, owner, _ := resetManagerFixture(t)
	delayed := &exportCapturedVault{Store: vault, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(delayed.finish)
	m.SetCredentialStore(delayed)
	done := make(chan error, 1)
	go func() { done <- m.CheckTOTPExport(owner.ID) }()
	select {
	case <-delayed.entered:
	case <-time.After(time.Second):
		t.Fatal("export did not reach the protected read")
	}
	changed := make(chan struct{})
	go func() { m.SetCredentialStore(credentials.NewMemoryStore()); close(changed) }()
	blocked := false
	select {
	case <-changed:
	case <-time.After(time.Second):
		blocked = true
	}
	delayed.finish()
	<-changed
	err := <-done
	if blocked {
		t.Error("protected credential I/O held the Manager lock")
	}
	if !errors.Is(err, ErrCredentialReadSuperseded) {
		t.Fatal("export accepted a read from a replaced credential store", err)
	}
}

func TestTOTPWorkflowExportUsesOneCredentialSnapshot(t *testing.T) {
	m, vault, owner, _ := strictWorkflowExportFixture(t)
	counted := &strictReviewCountingVault{Store: vault}
	m.SetCredentialStore(counted)
	if err := m.CheckTOTPWorkflowExport(owner.ID); err != nil {
		t.Fatal("valid active reset upload failed", err)
	}
	if counted.loads != 1 {
		t.Fatal("workflow export checked different credential snapshots", counted.loads)
	}
	if _, err := vault.Load("definitely-absent@example.test"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("synthetic store does not distinguish missing credentials", err)
	}
}

func TestTOTPExportRejectsChangedRelatedStateAfterVaultRead(t *testing.T) {
	for _, scenario := range []string{"owner-credentials", "history-credentials", "new-history", "workflow-finished", "unrelated-credentials", "new-journal"} {
		t.Run(scenario, func(t *testing.T) {
			m, vault, owner, peer := resetManagerFixture(t)
			if scenario == "workflow-finished" {
				m, vault, owner, peer = strictWorkflowExportFixture(t)
			}
			unrelated, err := m.Submit(Spec{ID: "unrelated-export-generation", Email: "different@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			delayed := &exportCapturedVault{Store: vault, entered: make(chan struct{}), release: make(chan struct{})}
			t.Cleanup(delayed.finish)
			m.SetCredentialStore(delayed)
			done := make(chan error, 1)
			go func() {
				if scenario == "workflow-finished" {
					done <- m.CheckTOTPWorkflowExport(owner.ID)
				} else {
					done <- m.CheckTOTPExport(owner.ID)
				}
			}()
			select {
			case <-delayed.entered:
			case <-time.After(time.Second):
				t.Fatal("export did not reach the controlled credential read")
			}
			var journalPath string
			var journalBefore []byte
			mutation := make(chan error, 1)
			go func() {
				switch scenario {
				case "owner-credentials", "history-credentials", "unrelated-credentials":
					id := owner.ID
					if scenario == "history-credentials" {
						id = peer.ID
					} else if scenario == "unrelated-credentials" {
						id = unrelated.ID
					}
					m.UpdateStoredCredentials(id, credentials.Credentials{Password: "synthetic-later-password"})
					mutation <- nil
				case "new-history":
					_, err := m.Submit(Spec{ID: "new-export-history", Email: owner.Email, ImportOnly: true})
					mutation <- err
				case "workflow-finished":
					mutation <- m.CompleteTOTPWorkflowStep(t.Context(), owner.ID, "upload")
				case "new-journal":
					journalPath = filepath.Join(m.store.Root, peer.ID, "totp-setup-result.json")
					err := protocol.WritePrivateJSON(journalPath, map[string]any{"unknown_operation": "synthetic-retained-evidence"})
					if err == nil {
						journalBefore, err = os.ReadFile(journalPath)
					}
					mutation <- err
				}
			}()
			select {
			case err := <-mutation:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				delayed.finish()
				<-mutation
				<-done
				t.Fatal("protected I/O blocked a local account update")
			}
			delayed.finish()
			err = <-done
			want := ErrCredentialReadSuperseded
			if scenario == "unrelated-credentials" {
				want = nil
			} else if scenario == "new-journal" {
				want = ErrTOTPRecoveryPending
			}
			if !errors.Is(err, want) {
				t.Fatalf("export used the wrong account generation: got=%v want=%v", err, want)
			}
			if journalPath != "" {
				after, err := os.ReadFile(journalPath)
				if err != nil || !bytes.Equal(journalBefore, after) {
					t.Fatal("read-only gate changed newly arrived recovery evidence", err)
				}
			}
		})
	}
}

func TestTOTPExportContextCancelsProtectedRead(t *testing.T) {
	for _, entry := range []string{"job", "email", "workflow"} {
		for _, source := range []string{"caller", "shutdown"} {
			t.Run(entry+"/"+source, func(t *testing.T) {
				m, vault, owner, _ := resetManagerFixture(t)
				delayed := &exportCapturedVault{Store: vault, entered: make(chan struct{}), release: make(chan struct{})}
				t.Cleanup(delayed.finish)
				m.SetCredentialStore(delayed)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					switch entry {
					case "job":
						done <- m.CheckTOTPExportContext(ctx, owner.ID)
					case "email":
						done <- m.CheckTOTPExportEmailContext(ctx, owner.Email)
					case "workflow":
						done <- m.CheckTOTPWorkflowExportContext(ctx, owner.ID)
					}
				}()
				select {
				case <-delayed.entered:
				case <-time.After(time.Second):
					t.Fatal("export did not begin a cancelable credential read")
				}
				if source == "caller" {
					cancel()
				} else {
					ctx, stop := context.WithTimeout(t.Context(), time.Second)
					defer stop()
					if err := m.Shutdown(ctx); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case err := <-done:
					if err != context.Canceled {
						t.Fatal("cancellation lost its safe exact outcome", err)
					}
				case <-time.After(time.Second):
					delayed.finish()
					<-done
					t.Fatal("export did not cancel the protected read")
				}
			})
		}
	}
}

type exportBlockingProtector struct {
	credentials.StateProtector
	entered chan struct{}
	once    sync.Once
}

func (p *exportBlockingProtector) OpenContext(ctx context.Context, _ string, _, _ []byte) ([]byte, error) {
	p.once.Do(func() { close(p.entered) })
	<-ctx.Done()
	return nil, errors.Join(ctx.Err(), errors.New("synthetic-private-journal-diagnostic"))
}

func TestTOTPExportContextCancelsJournalReadWithoutHoldingManager(t *testing.T) {
	m, _, owner, _ := resetManagerFixture(t)
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	if err := privatestate.Register(m.store.Root, protector); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.store.Root, owner.ID, "totp-setup-result.json")
	if err := privatestate.WriteJSON(t.Context(), path, "totp-reset", owner.Email, "synthetic-pending-operation", map[string]any{"email": owner.Email, "operation_id": "synthetic-pending-operation"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &exportBlockingProtector{StateProtector: protector, entered: make(chan struct{})}
	if err := privatestate.Register(m.store.Root, blocked); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = privatestate.Register(m.store.Root, protector) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.CheckTOTPExportContext(ctx, owner.ID) }()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("export did not reach protected journal decryption")
	}
	listed := make(chan struct{})
	go func() { m.List(); close(listed) }()
	select {
	case <-listed:
	case <-time.After(time.Second):
		cancel()
		<-done
		<-listed
		t.Fatal("protected journal I/O held the Manager lock")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal("journal cancellation exposed a private diagnostic or lost cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("journal decryptor did not inherit export cancellation")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled journal read changed protected evidence", err)
	}
}
