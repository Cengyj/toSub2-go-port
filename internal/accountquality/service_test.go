package accountquality

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/store"
)

type fixture struct {
	mu       sync.Mutex
	accounts []Account
	retained []string
	probe    func(context.Context, modelprobe.Options) (modelprobe.Result, error)
	calls    atomic.Int32
	released atomic.Int32
}

func fixtureProvider(f *fixture) Provider {
	return Provider{Accounts: func() []Account { f.mu.Lock(); defer f.mu.Unlock(); return append([]Account(nil), f.accounts...) }, RetainedIDs: func() []string { f.mu.Lock(); defer f.mu.Unlock(); return append([]string(nil), f.retained...) }, Acquire: func(ctx context.Context, id string) (*Lease, error) {
		f.calls.Add(1)
		f.mu.Lock()
		run := f.probe
		f.mu.Unlock()
		return &Lease{Context: ctx, Identity: "grant-" + id, Validate: func() error { return nil }, Release: func() { f.released.Add(1) }, Probe: run}, nil
	}}
}
func resultFor(c modelprobe.Options, score int) modelprobe.Result {
	verdict := "normal"
	if score < c.MinimumScore {
		verdict = "degraded"
	}
	return modelprobe.Result{Model: c.Model, RuleVersion: modelprobe.RuleVersion, Verdict: verdict, Score: score, Total: modelprobe.TotalQuestions, CheckedAt: time.Now().UTC(), DurationMS: 1}
}
func newFixture(t *testing.T) (*Service, *fixture, string) {
	t.Helper()
	root := t.TempDir()
	f := &fixture{accounts: []Account{{ID: "a", Key: "email-a", Revision: "r1", CanCheck: true}}, retained: []string{"a"}, probe: func(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) { return resultFor(o, 1), nil }}
	s, err := New(root, fixtureProvider(f))
	if err != nil {
		t.Fatal(err)
	}
	enable(t, s, nil)
	s.Start(context.Background())
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		if err := s.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s, f, root
}
func enable(t *testing.T, s *Service, edit func(*Settings)) {
	t.Helper()
	c, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	c.Enabled = true
	c.BeforeUpload = true
	c.Model = "synthetic-model"
	c.CooldownSeconds = 0
	if edit != nil {
		edit(&c)
	}
	if _, err = s.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}
func waitFor(t *testing.T, test func() bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if test() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not complete")
}
func idle(t *testing.T, s *Service) {
	t.Helper()
	waitFor(t, func() bool { st := s.Status(); return st.Active == 0 && st.Queued == 0 })
}
func upload(identity string, run func(context.Context, modelprobe.Options) (modelprobe.Result, error)) UploadProbe {
	return UploadProbe{Identity: identity, Validate: func() error { return nil }, Run: run}
}
func good(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) {
	return resultFor(o, 1), nil
}

func TestReadSettingsOnlyMissingDefaults(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	c, err := ReadSettings(root)
	if err != nil || c.Enabled || c.BeforeUpload || !c.BlockFailed || !c.BlockDegraded || c.MinimumScore != modelprobe.DefaultMinimumScore || c.TimeoutSeconds != 90 {
		t.Fatalf("defaults %#v %v", c, err)
	}
	if _, err = os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created a directory")
	}
	for _, body := range []string{`{}`, `null`, `{"schemaVersion":1}`, `{"revision":1}`, `{"schemaVersion":null,"revision":1}`, `{"schemaVersion":1,"revision":null}`, `invalid`} {
		t.Run(body, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, directoryName)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, settingsName), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSettings(root); !errors.Is(err, ErrSettingsUnavailable) {
				t.Fatalf("bad config accepted: %v", err)
			}
		})
	}
}

func TestSettingsFailureCannotBecomeDisabled(t *testing.T) {
	s, _, root := newFixture(t)
	saved := s.persist
	c, _ := s.Settings()
	c.Enabled = false
	s.persist = func(context.Context, string, any) error { return errors.New("synthetic disk failure") }
	failed, err := s.Update(context.Background(), c)
	if !errors.Is(err, ErrPersistence) {
		t.Fatal(err)
	}
	if _, err = s.Settings(); err == nil {
		t.Fatal("reload cleared an unconfirmed save")
	}
	if d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err == nil || d.Allowed {
		t.Fatal("failed policy permitted upload")
	}
	s.persist = saved
	// The failed write returns the still-effective old policy. Explicitly
	// reapply the intended disable; a failed save must not claim it was saved.
	failed.Enabled = false
	if _, err = s.Update(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	if d, err := s.CheckForUpload(context.Background(), "a", UploadProbe{}); err != nil || !d.Allowed || d.Checked {
		t.Fatalf("confirmed disable: %#v %v", d, err)
	}
	if err = os.WriteFile(filepath.Join(root, directoryName, settingsName), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if d, err := s.CheckForUpload(context.Background(), "a", UploadProbe{}); err == nil || d.Allowed {
		t.Fatal("broken off policy permitted upload")
	}
}

func TestUncertainPolicyRequiresExplicitSave(t *testing.T) {
	s, _, _ := newFixture(t)
	write := s.persist
	s.persist = func(ctx context.Context, name string, value any) error {
		if err := write(ctx, name, value); err != nil {
			return err
		}
		return store.ErrDurabilityUncertain
	}
	c, _ := s.Settings()
	c.TimeoutSeconds = 91
	current, err := s.Update(context.Background(), c)
	if !errors.Is(err, ErrPersistenceUncertain) {
		t.Fatal(err)
	}
	s.persist = write
	if _, err = s.Settings(); err == nil {
		t.Fatal("read alone confirmed durability")
	}
	if _, err = s.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Settings(); err != nil {
		t.Fatal(err)
	}
}

func TestQueueBoundAndSameAccountMerge(t *testing.T) {
	s, f, _ := newFixture(t)
	s.runGate <- struct{}{}
	defer func() { <-s.runGate }()
	f.mu.Lock()
	f.accounts = nil
	for n := 0; n < 1001; n++ {
		id := fmt.Sprintf("id-%04d", n)
		f.accounts = append(f.accounts, Account{ID: id, Key: id, Revision: "r", CanCheck: true})
	}
	f.mu.Unlock()
	r, err := s.Refresh(context.Background(), nil, true)
	if err != nil || len(r.Accepted) != 1000 || len(r.Skipped) != 1 {
		t.Fatalf("bounded receipt: %d %d %v", len(r.Accepted), len(r.Skipped), err)
	}
	r2, err := s.Refresh(context.Background(), []string{r.Accepted[0], r.Accepted[0]}, false)
	if err != nil || len(r2.Merged) != 1 {
		t.Fatalf("dedup %#v %v", r2, err)
	}
	if _, err = s.Cancel(context.Background(), r.Accepted); err != nil {
		t.Fatal(err)
	}
}

func TestManualSameEmailDeduplicatesHistoricalIDs(t *testing.T) {
	s, f, _ := newFixture(t)
	s.runGate <- struct{}{}
	defer func() { <-s.runGate }()
	f.mu.Lock()
	f.accounts = append(f.accounts, Account{ID: "b", Key: "email-a", Revision: "r2", CanCheck: true})
	f.mu.Unlock()
	r, err := s.Refresh(context.Background(), []string{"a", "b"}, false)
	if err != nil || len(r.Accepted) != 1 || len(r.Merged) != 1 {
		t.Fatalf("same-account receipt %#v %v", r, err)
	}
	_, _ = s.Cancel(context.Background(), []string{"a"})
}

func TestUploadTakesOverQueuedManualWithoutAcquire(t *testing.T) {
	s, f, _ := newFixture(t)
	s.runGate <- struct{}{}
	if _, err := s.Refresh(context.Background(), []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	finished := make(chan error, 1)
	go func() {
		d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
			calls.Add(1)
			return good(ctx, o)
		}))
		if err == nil && !d.Allowed {
			err = errors.New("not allowed")
		}
		finished <- err
	}()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.flights["email-a"] != nil && s.flights["email-a"].executor == "upload"
	})
	<-s.runGate
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	idle(t, s)
	if f.calls.Load() != 0 || calls.Load() != 1 {
		t.Fatalf("acquired %d, upload probes %d", f.calls.Load(), calls.Load())
	}
}

func TestRunningManualAndUploadShareExactIdentity(t *testing.T) {
	s, f, _ := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.probe = func(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) {
		close(started)
		<-release
		return resultFor(o, 1), nil
	}
	f.mu.Unlock()
	if _, err := s.Refresh(context.Background(), []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	<-started
	var uploadCalls atomic.Int32
	done := make(chan error, 1)
	go func() {
		d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
			uploadCalls.Add(1)
			return good(ctx, o)
		}))
		if err == nil && !d.Allowed {
			err = ErrUploadBlocked
		}
		done <- err
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	idle(t, s)
	if f.calls.Load() != 1 || f.released.Load() != 1 || uploadCalls.Load() != 0 {
		t.Fatal("shared probe acquired or ran twice")
	}
}

func TestUploadFreshCacheBindsIdentityAndOptions(t *testing.T) {
	s, _, _ := newFixture(t)
	var calls atomic.Int32
	probe := func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
		calls.Add(1)
		return good(ctx, o)
	}
	for n := 0; n < 2; n++ {
		d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", probe))
		if err != nil || !d.Allowed || d.Cached != (n == 1) {
			t.Fatalf("cache %#v %v", d, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("did not reuse fresh result")
	}
	if _, err := s.CheckForUpload(context.Background(), "a", upload("grant-b", probe)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("different identity reused cache")
	}
	c, _ := s.Settings()
	c.ReasoningEffort = "medium"
	if _, err := s.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckForUpload(context.Background(), "a", upload("grant-b", probe)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("different options reused cache")
	}
}

func TestLatestFailurePreservesHistoryButCannotAuthorize(t *testing.T) {
	s, f, root := newFixture(t)
	if _, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.probe = func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		return modelprobe.Result{}, &modelprobe.Error{Code: "invalid_response"}
	}
	f.mu.Unlock()
	if _, err := s.Refresh(context.Background(), []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	idle(t, s)
	snaps, _ := s.Snapshots([]string{"a"})
	if snaps["a"].LastAttempt.Status != "failed" || snaps["a"].LastResult == nil || snaps["a"].LastResult.Verdict != "normal" {
		t.Fatalf("history lost %#v", snaps["a"])
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := New(root, fixtureProvider(f))
	if err != nil {
		t.Fatal(err)
	}
	restored.Start(context.Background())
	defer restored.Close(context.Background())
	var calls atomic.Int32
	d, err := restored.CheckForUpload(context.Background(), "a", upload("grant-a", func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		calls.Add(1)
		return modelprobe.Result{}, &modelprobe.Error{Code: "network_error"}
	}))
	if err == nil || d.Allowed || d.Cached || calls.Load() != 1 {
		t.Fatalf("old normal authorized %#v %v calls=%d", d, err, calls.Load())
	}
}

func TestFailedFinalWriteLeavesDurableAttemptBarrier(t *testing.T) {
	s, f, root := newFixture(t)
	if _, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err != nil {
		t.Fatal(err)
	}
	write := s.persist
	s.persist = func(ctx context.Context, name string, value any) error {
		if row, ok := value.(storedRow); ok && row.Snapshot.LastAttempt.Status != "running" {
			return errors.New("synthetic final disk failure")
		}
		return write(ctx, name, value)
	}
	f.mu.Lock()
	f.probe = func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		return modelprobe.Result{}, errors.New("synthetic-private-reply")
	}
	f.mu.Unlock()
	if _, err := s.Refresh(context.Background(), []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	idle(t, s)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.persist = write
	restored, err := New(root, fixtureProvider(f))
	if err != nil {
		t.Fatal(err)
	}
	restored.Start(context.Background())
	defer restored.Close(context.Background())
	snaps, _ := restored.Snapshots([]string{"a"})
	if snaps["a"].LastAttempt.Status != "cancelled" {
		t.Fatalf("old success survived failed write %#v", snaps["a"])
	}
	var calls atomic.Int32
	d, err := restored.CheckForUpload(context.Background(), "a", upload("grant-a", func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
		calls.Add(1)
		return good(ctx, o)
	}))
	if err != nil || !d.Allowed || d.Cached || calls.Load() != 1 {
		t.Fatalf("did not recheck: %#v %v", d, err)
	}
}

func TestFailureAndDegradedPolicies(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		t.Run(fmt.Sprint(degraded), func(t *testing.T) {
			s, _, _ := newFixture(t)
			c, _ := s.Settings()
			c.BlockDegraded = false
			c.BlockFailed = false
			if _, err := s.Update(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) {
				if degraded {
					return resultFor(o, 0), nil
				}
				return modelprobe.Result{}, &modelprobe.Error{Code: "network_error"}
			}))
			if err != nil || !d.Allowed || !d.Checked {
				t.Fatalf("explicit allow policy %#v %v", d, err)
			}
			if !degraded && d.Result != nil {
				t.Fatal("failure became zero-score result")
			}
		})
	}
}

func TestCloseWaitsBorrowedUploadAndNeverStartsAnother(t *testing.T) {
	s, _, _ := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) {
			close(started)
			<-release
			return resultFor(o, 1), nil
		}))
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close returned before borrowed probe ended: %v", err)
	}
	if d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err == nil || d.Allowed {
		t.Fatal("closed service admitted upload")
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("cancelled upload allowed")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTrashRetentionAndExplicitPurge(t *testing.T) {
	s, f, root := newFixture(t)
	if _, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeAccountIDs(context.Background(), []string{"a"}); !errors.Is(err, ErrCacheBusy) {
		t.Fatal("active account purged", err)
	}
	f.mu.Lock()
	f.accounts = nil
	f.mu.Unlock()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := New(root, fixtureProvider(f))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(context.Background())
	rows, _ := restored.Snapshots([]string{"a"})
	if rows["a"].LastResult == nil || rows["a"].CanCheck {
		t.Fatal("trash history lost or admitted")
	}
	if err := restored.PurgeAccountIDs(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, directoryName, snapshotFile("a"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cache file remains")
	}
}

func TestPersistedResultsNeverContainPrivateProbeData(t *testing.T) {
	s, _, root := newFixture(t)
	secret := "synthetic-token-and-private-answer"
	_, _ = s.CheckForUpload(context.Background(), "a", upload(secret, func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		return modelprobe.Result{}, errors.New(secret)
	}))
	entries, err := os.ReadDir(filepath.Join(root, directoryName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		b, err := os.ReadFile(filepath.Join(root, directoryName, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), secret) || strings.Contains(string(b), "access_token") {
			t.Fatal("private probe data persisted")
		}
	}
}
