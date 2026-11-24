package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func sharedVaultWriterManager(t *testing.T, vault credentials.Store, origin string) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	m.SetCredentialStore(vault)
	m.SetAccountOperationLease(func(email string) (func() error, error) { return accountlock.Acquire(origin, email) })
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m, st
}

// The peer is a real test subprocess holding the same host-wide lock as a
// runner with a different output root. It never reads any production data.
func TestSharedVaultStructuredWritesHonorPeerAccountLease(t *testing.T) {
	for _, entry := range []string{"submit", "update", "retry", "queue", "import-new", "import-existing"} {
		t.Run(entry, func(t *testing.T) {
			const origin, email = "https://shared-writer-lock.example.test", "shared-writer@example.test"
			vault := &sharedWriterCountingVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, origin)
			var existing *Job
			if entry != "submit" && entry != "import-new" {
				var err error
				existing, err = m.Submit(Spec{ID: "existing", Email: email, ImportOnly: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			saved := credentials.Credentials{Password: "original-password", TOTPSecret: "ACTIVESEED", TOTPFactorID: "active-factor", MailBody: "original-body"}
			if err := vault.MemoryStore.Save(email, saved); err != nil {
				t.Fatal(err)
			}
			beforeJobs := m.List()
			metaPath := filepath.Join(st.Root, "job-meta.json")
			beforeMeta, beforeMetaErr := os.ReadFile(metaPath)
			stopPeer := holdAccountQueryProcessLock(t, "https://SHARED-WRITER-LOCK.example.test:443/peer-root", " SHARED-WRITER@EXAMPLE.TEST ")
			if existing != nil {
				unchanged, created, err := m.SubmitUniqueEmail(Spec{Email: email, ImportOnly: true})
				if err != nil || created || unchanged.ID != existing.ID || vault.saves.Load() != 0 {
					t.Fatal("read-only duplicate email import changed its no-write contract", err)
				}
			}
			values := map[string]any{"mailRequestBody": "operator-edited-body"}
			action := func() error {
				var err error
				switch entry {
				case "submit":
					_, err = m.Submit(Spec{ID: "new-operation", Email: email, Input: values})
				case "update":
					_, err = m.Update(existing.ID, values)
				case "retry":
					_, err = m.RetryWithValues(existing.ID, values)
				case "queue":
					_, err = m.QueueOperation(existing.ID, "relogin", values)
				case "import-new", "import-existing":
					_, _, err = m.SubmitUniqueEmail(Spec{Email: email, ImportOnly: true, Input: values})
				}
				return err
			}
			err := action()
			if err == nil || vault.saves.Load() != 0 {
				t.Errorf("credential write ignored a peer runner's account ownership: refused=%v vault_writes=%d", err != nil, vault.saves.Load())
			}
			got, loadErr := vault.Load(email)
			if loadErr != nil || !credentials.EqualReadback(saved, got) {
				t.Error("busy account credentials were overwritten", loadErr)
			}
			if !reflect.DeepEqual(beforeJobs, m.List()) {
				t.Error("rejected or unowned credential write changed account/operation state")
			}
			afterMeta, afterMetaErr := os.ReadFile(metaPath)
			if !bytes.Equal(beforeMeta, afterMeta) || errors.Is(beforeMetaErr, os.ErrNotExist) != errors.Is(afterMetaErr, os.ErrNotExist) {
				t.Error("unowned credential write committed metadata before account ownership")
			}
			stopPeer()
			if err != nil {
				if retryErr := action(); retryErr != nil {
					t.Fatal("released peer lock still blocked the requested credential edit", retryErr)
				}
				want := saved
				want.MailBody = "operator-edited-body"
				got, loadErr := vault.Load(email)
				if loadErr != nil || !credentials.EqualReadback(want, got) {
					t.Fatal("normal edit after peer completion lost existing credentials", loadErr)
				}
			}
		})
	}
}

type sharedWriterCountingVault struct {
	*credentials.MemoryStore
	saves atomic.Int32
}

func (v *sharedWriterCountingVault) Save(email string, value credentials.Credentials) error {
	v.saves.Add(1)
	return v.MemoryStore.Save(email, value)
}

type sharedWriterSaveGate struct {
	entered  chan struct{}
	proceed  chan struct{}
	once     sync.Once
	finished chan struct{}
}

func (g *sharedWriterSaveGate) release() { g.once.Do(func() { close(g.proceed) }) }

type sharedWriterGatedVault struct {
	*credentials.MemoryStore
	mu   sync.Mutex
	gate *sharedWriterSaveGate
}

func (v *sharedWriterGatedVault) Save(email string, value credentials.Credentials) error {
	return v.SaveContext(context.Background(), email, value)
}

func (v *sharedWriterGatedVault) SaveContext(ctx context.Context, email string, value credentials.Credentials) error {
	v.mu.Lock()
	gate := v.gate
	v.gate = nil
	v.mu.Unlock()
	if gate != nil {
		close(gate.entered)
		select {
		case <-gate.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return v.MemoryStore.Save(email, value)
}

func (v *sharedWriterGatedVault) LoadContext(ctx context.Context, email string) (credentials.Credentials, error) {
	if err := ctx.Err(); err != nil {
		return credentials.Credentials{}, err
	}
	return v.MemoryStore.Load(email)
}

func (v *sharedWriterGatedVault) DeleteContext(ctx context.Context, email string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.MemoryStore.Delete(email)
}

// Delay the import at the real credential Store boundary, after its merge has
// read the old record. A simulated runner publishes new material while a real
// peer subprocess owns the account lease. Import must never reach this stale
// write, or erase the runner's later evidence when the delayed save resumes.
func TestSharedVaultImportCannotOverwritePeerCommit(t *testing.T) {
	for _, committed := range []string{"pending-enrollment", "confirmed-password-and-seed"} {
		t.Run(committed, func(t *testing.T) {
			const origin, email = "https://shared-writer-race.example.test", "shared-writer-race@example.test"
			vault := &sharedWriterGatedVault{MemoryStore: credentials.NewMemoryStore()}
			m, st := sharedVaultWriterManager(t, vault, origin)
			job, err := m.Submit(Spec{ID: "imported", Email: email, ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			original := credentials.Credentials{Password: "original-password", TOTPSecret: "OLDSEED", TOTPFactorID: "old-factor", MailBody: "original-body"}
			if err := vault.MemoryStore.Save(email, original); err != nil {
				t.Fatal(err)
			}
			stopPeer := holdAccountQueryProcessLock(t, origin, email)
			gate := &sharedWriterSaveGate{entered: make(chan struct{}), proceed: make(chan struct{}), finished: make(chan struct{})}
			vault.mu.Lock()
			vault.gate = gate
			vault.mu.Unlock()
			result := make(chan error, 1)
			go func() {
				defer close(gate.finished)
				_, _, err := m.SubmitUniqueEmail(Spec{Email: email, ImportOnly: true, Input: map[string]any{"mailRequestBody": "operator-edited-body"}})
				result <- err
			}()
			t.Cleanup(func() {
				gate.release()
				select {
				case <-gate.finished:
				case <-time.After(time.Second):
					t.Error("credential import goroutine did not exit")
				}
			})
			var actionErr error
			blocked := false
			select {
			case <-gate.entered:
				blocked = true
			case actionErr = <-result:
				if actionErr == nil {
					t.Fatal("import bypassed both the account lease and credential Store")
				}
			case <-time.After(time.Second):
				t.Fatal("credential edit neither returned nor reached the Store boundary")
			}
			peer := original
			if committed == "pending-enrollment" {
				peer.PendingTOTPSecret, peer.PendingTOTPURI, peer.PendingTOTPFactorID = "PEERCANDIDATE", "peer-uri", "peer-factor"
			} else {
				peer.Password, peer.TOTPSecret, peer.TOTPFactorID = "peer-confirmed-password", "PEERCONFIRMEDSEED", "peer-confirmed-factor"
				peer.TOTPResetOperationID = "peer-completed-reset"
			}
			if err := vault.MemoryStore.Save(email, peer); err != nil {
				t.Fatal(err)
			}
			gate.release()
			if blocked {
				select {
				case actionErr = <-result:
				case <-time.After(time.Second):
					t.Fatal("delayed import did not finish")
				}
			}
			retained, err := vault.Load(email)
			if err != nil || !credentials.EqualReadback(peer, retained) {
				t.Errorf("save-only import overwrote a peer runner commit: kind=%s retained_pending=%v retained_active_password=%v retained_active_seed=%v", committed, retained.PendingTOTPSecret == peer.PendingTOTPSecret && retained.PendingTOTPURI == peer.PendingTOTPURI && retained.PendingTOTPFactorID == peer.PendingTOTPFactorID, retained.Password == peer.Password, retained.TOTPSecret == peer.TOTPSecret && retained.TOTPFactorID == peer.TOTPFactorID)
			}
			if blocked || actionErr == nil {
				t.Error("same-account import entered a credential write while another process owned the account")
			}
			// No synthetic runner was installed on this manager. This proves a
			// local import alone can reach the overwrite, before authentication.
			after, ok := m.Get(job.ID)
			if !ok || after.Attempts != 0 || after.Status != StatusImported {
				t.Fatal("fixture unexpectedly started an authentication operation")
			}
			if _, err := os.Stat(filepath.Join(st.Root, job.ID, "oauth-grant.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fixture unexpectedly produced OAuth output", err)
			}
			stopPeer()
		})
	}
}
