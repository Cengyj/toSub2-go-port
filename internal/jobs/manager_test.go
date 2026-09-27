package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

type memoryVault struct {
	values map[string]credentials.Credentials
}

func (v *memoryVault) Save(e string, c credentials.Credentials) error {
	if v.values == nil {
		v.values = map[string]credentials.Credentials{}
	}
	v.values[e] = c
	return nil
}
func (v *memoryVault) Load(e string) (credentials.Credentials, error) {
	c, ok := v.values[e]
	if !ok {
		return credentials.Credentials{}, os.ErrNotExist
	}
	return c, nil
}
func (v *memoryVault) Delete(e string) error { delete(v.values, e); return nil }

func TestRestoreLegacyDirectoryAndRedactsSetupSecret(t *testing.T) {
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "legacy-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	meta := `{"email":"legacy@example.com","status":"starting","prompt":"continue","has_password":true}`
	if err := os.WriteFile(filepath.Join(dir, "job-meta.json"), []byte(meta), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), 1, st, nil)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, ok := m.Get("legacy-1")
	if !ok || j.Email != "legacy@example.com" || j.Status != "reauth_required" {
		t.Fatalf("restored=%#v ok=%v", j, ok)
	}
	j.TOTPSetupSecret = "SECRET"
	m.mu.Lock()
	m.jobs[j.ID].TOTPSetupSecret = "SECRET"
	m.persistLocked()
	m.mu.Unlock()
	b, _ := os.ReadFile(filepath.Join(root, "job-meta.json"))
	if string(b) == "" || strings.Contains(string(b), "SECRET") {
		t.Fatal("secret persisted")
	}
	var x any
	if json.Unmarshal(b, &x) != nil {
		t.Fatal("invalid metadata")
	}
}

func TestQueueOperationIsAtomic(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, _ := m.Submit(Spec{Email: "a@example.com"})
	if _, err := m.QueueOperation(j.ID, "relogin", map[string]any{"sub2apiBaseURL": "https://example"}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(j.ID)
	if got.Operation != "relogin" || got.Status != StatusQueued {
		t.Fatalf("%#v", got)
	}
}

func TestRestoreRecoversPrivatePasswordAndTOTPResults(t *testing.T) {
	root := t.TempDir()
	st, _ := store.New(root)
	dir := filepath.Join(root, "j1")
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, "job-meta.json"), []byte(`{"id":"j1","email":"recover@example.com","status":"resume_available"}`), 0600)
	_ = os.WriteFile(filepath.Join(dir, "password-add-result.json"), []byte(`{"version":1,"email":"recover@example.com","password":"Strong-Password-123!","added_at":"2026-01-01T00:00:00Z"}`), 0600)
	_ = os.WriteFile(filepath.Join(dir, "totp-setup-result.json"), []byte(`{"email":"recover@example.com","secret":"JBSWY3DPEHPK3PXP","activation_succeeded":true}`), 0600)
	v := &memoryVault{values: map[string]credentials.Credentials{}}
	m := NewManager(context.Background(), 1, st, nil)
	m.SetCredentialStore(v)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, ok := m.Get("j1")
	if !ok || !j.HasPassword || !j.HasTOTPKey {
		t.Fatalf("job=%#v", j)
	}
	if _, e := os.Stat(filepath.Join(dir, "password-add-result.json")); !os.IsNotExist(e) {
		t.Fatal("password result not recovered")
	}
	if _, e := os.Stat(filepath.Join(dir, "totp-setup-result.json")); !os.IsNotExist(e) {
		t.Fatal("totp result not recovered")
	}
}

func waitForStatus(t *testing.T, m *Manager, id, want string) *Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := m.Get(id); ok && j.Status == want {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s", id, want)
	return nil
}

func TestManagerConcurrencyAndEmailLock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, 2, nil, nil)
	var active, peak int32
	var mu sync.Mutex
	runningEmail := map[string]bool{}
	m.SetRunner(func(ctx context.Context, j *Job, _ func(JobEvent)) error {
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
				break
			}
		}
		mu.Lock()
		if j.Email != "" && runningEmail[j.Email] {
			mu.Unlock()
			return context.Canceled
		}
		runningEmail[j.Email] = true
		mu.Unlock()
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		mu.Lock()
		delete(runningEmail, j.Email)
		mu.Unlock()
		atomic.AddInt32(&active, -1)
		return nil
	})
	m.Start()
	defer m.Shutdown(context.Background())
	var ids []string
	for i := 0; i < 6; i++ {
		email := "a@example.com"
		if i%2 == 1 {
			email = "b@example.com"
		}
		j, e := m.Submit(Spec{Email: email})
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, j.ID)
	}
	for _, id := range ids {
		waitForStatus(t, m, id, StatusSucceeded)
	}
	if got := atomic.LoadInt32(&peak); got > 2 {
		t.Fatalf("peak concurrency %d", got)
	}
}

func TestManagerCancelQueuedAndRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, 1, nil, nil)
	m.SetRunner(func(ctx context.Context, _ *Job, _ func(JobEvent)) error { <-ctx.Done(); return ctx.Err() })
	m.Start()
	first, _ := m.Submit(Spec{Email: "one@example.com"})
	second, _ := m.Submit(Spec{Email: "two@example.com"})
	time.Sleep(10 * time.Millisecond)
	if !m.Cancel(second.ID) {
		t.Fatal("queued cancel failed")
	}
	waitForStatus(t, m, second.ID, StatusCanceled)
	if !m.Cancel(first.ID) {
		t.Fatal("running cancel failed")
	}
	waitForStatus(t, m, first.ID, StatusCanceled)
	_ = m.Shutdown(context.Background())
}
