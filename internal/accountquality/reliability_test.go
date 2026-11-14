package accountquality

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

func TestCancelledFlightRemainsBusyUntilTransportReleases(t *testing.T) {
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
	f.mu.Lock()
	f.accounts = nil
	f.mu.Unlock()
	r, err := s.Cancel(context.Background(), []string{"a"})
	if err != nil || len(r.Cancelled) != 1 {
		t.Fatalf("removed account cancellation %#v %v", r, err)
	}
	if st := s.Status(); st.Active != 1 {
		t.Fatal("cancel released live transport ownership")
	}
	if err := s.PurgeAccountIDs(context.Background(), []string{"a"}); !errors.Is(err, ErrCacheBusy) {
		t.Fatal("live cache purged", err)
	}
	close(release)
	idle(t, s)
	if f.released.Load() != 1 {
		t.Fatal("transport not released")
	}
	if err := s.PurgeAccountIDs(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
}

func TestBlockFailedFalseNeverBypassesPersistenceFailure(t *testing.T) {
	for _, stage := range []string{"start", "finish"} {
		t.Run(stage, func(t *testing.T) {
			s, _, _ := newFixture(t)
			c, _ := s.Settings()
			c.BlockFailed = false
			if _, err := s.Update(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			write := s.persist
			s.persist = func(ctx context.Context, name string, value any) error {
				if row, ok := value.(storedRow); ok && (stage == "start" && row.Snapshot.LastAttempt.Status == "running" || stage == "finish" && row.Snapshot.LastAttempt.Status != "running") {
					return errors.New("synthetic durability failure")
				}
				return write(ctx, name, value)
			}
			var calls atomic.Int32
			d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
				calls.Add(1)
				return good(ctx, o)
			}))
			if err == nil || d.Allowed || !errors.Is(err, ErrCachePersistence) {
				t.Fatalf("persistence bypassed %#v %v", d, err)
			}
			if stage == "start" && calls.Load() != 0 || stage == "finish" && calls.Load() != 1 {
				t.Fatal("unexpected network execution")
			}
		})
	}
}

func TestPolicyRequiresEveryFieldAndRejectsUnknownDuplicateOrNull(t *testing.T) {
	s, _, root := newFixture(t)
	c, _ := s.Settings()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]json.RawMessage
	if err = json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, directoryName, settingsName)
	for key := range values {
		if key == "persistenceWarning" {
			continue
		}
		t.Run("missing-"+key, func(t *testing.T) {
			copy := map[string]json.RawMessage{}
			for k, v := range values {
				if k != key {
					copy[k] = v
				}
			}
			bad, _ := json.Marshal(copy)
			if err := os.WriteFile(path, bad, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSettings(root); !errors.Is(err, ErrSettingsUnavailable) {
				t.Fatalf("missing %s accepted: %v", key, err)
			}
		})
	}
	for name, bad := range map[string][]byte{"null-enabled": append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"enabled":null}`)...), "unknown": append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"enable":false}`)...), "duplicate": append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"enabled":false}`)...)} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, bad, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSettings(root); !errors.Is(err, ErrSettingsUnavailable) {
				t.Fatal("ambiguous policy accepted", err)
			}
		})
	}
}

func TestStaticReadRejectsUnsafeDirectoryWithoutRepairingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission check")
	}
	_, _, root := newFixture(t)
	dir := filepath.Join(root, directoryName)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSettings(root); !errors.Is(err, ErrSettingsUnavailable) {
		t.Fatal("unsafe directory accepted")
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("read silently repaired directory")
	}
}

func TestProbeAdmissionFailuresRemainBlockedWithAllowPolicy(t *testing.T) {
	for _, code := range []string{"authorization_unavailable", "authorization_expired", "proxy_selection_required", "canceled"} {
		t.Run(code, func(t *testing.T) {
			s, _, _ := newFixture(t)
			c, _ := s.Settings()
			c.BlockFailed = false
			if _, err := s.Update(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
				return modelprobe.Result{}, &modelprobe.Error{Code: code}
			}))
			if err == nil || d.Allowed {
				t.Fatalf("unsafe admission permitted %s", code)
			}
			rows, _ := s.Snapshots([]string{"a"})
			if rows["a"].LastAttempt.Message == "模型检测未取得有效结果" {
				t.Fatal("specific diagnostic lost")
			}
		})
	}
}

func TestManualQueueRunsOnlyOneNetworkProbe(t *testing.T) {
	s, f, _ := newFixture(t)
	f.mu.Lock()
	f.accounts = append(f.accounts, Account{ID: "b", Key: "email-b", Revision: "r1", CanCheck: true})
	f.retained = append(f.retained, "b")
	f.mu.Unlock()
	started, release := make(chan struct{}, 2), make(chan struct{})
	var active, maximum atomic.Int32
	f.mu.Lock()
	f.probe = func(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) {
		n := active.Add(1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return resultFor(o, 1), nil
	}
	f.mu.Unlock()
	r, err := s.Refresh(context.Background(), nil, true)
	if err != nil || len(r.Accepted) != 2 {
		t.Fatal(r, err)
	}
	<-started
	if st := s.Status(); st.Active != 1 || st.Queued != 1 {
		t.Fatalf("not serial %#v", st)
	}
	close(release)
	idle(t, s)
	if maximum.Load() != 1 || f.calls.Load() != 2 {
		t.Fatal("parallel probes or dropped queue")
	}
}

func TestStartMarkerWriteFailurePreventsNetworkProbe(t *testing.T) {
	s, _, _ := newFixture(t)
	s.persist = func(context.Context, string, any) error { return errors.New("synthetic disk unavailable") }
	var calls atomic.Int32
	d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
		calls.Add(1)
		return good(ctx, o)
	}))
	if err == nil || d.Allowed || calls.Load() != 0 {
		t.Fatalf("network ran without durable attempt %#v %v %d", d, err, calls.Load())
	}
}

func TestDefaultDegradedPolicyBlocksAndKeepsRealScore(t *testing.T) {
	s, _, _ := newFixture(t)
	d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) { return resultFor(o, 0), nil }))
	if !errors.Is(err, ErrUploadBlocked) || d.Allowed || d.Result == nil || d.Result.Score != 0 || d.Result.Verdict != "degraded" {
		t.Fatalf("degraded decision %#v %v", d, err)
	}
	var outcome interface{ UploadOutcomeUncertain() bool }
	if !errors.As(err, &outcome) || outcome.UploadOutcomeUncertain() {
		t.Fatal("blocked upload described as remote uncertainty")
	}
	if d, err = s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err == nil || !d.Cached || d.Result.Score != 0 {
		t.Fatal("degraded cached result was treated as normal")
	}
}

func TestInvalidResultIsFailureNotZeroOrNormal(t *testing.T) {
	s, _, _ := newFixture(t)
	d, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", func(_ context.Context, o modelprobe.Options) (modelprobe.Result, error) {
		r := resultFor(o, 1)
		r.Total = 0
		return r, nil
	}))
	if err == nil || d.Allowed || d.Result != nil {
		t.Fatal("invalid result accepted")
	}
	rows, _ := s.Snapshots([]string{"a"})
	if rows["a"].LastResult != nil || rows["a"].LastAttempt.Status != "failed" {
		t.Fatal("failure became scored result")
	}
}

func TestSettingsReadHonorsUploadCancellationBudget(t *testing.T) {
	s, _, _ := newFixture(t)
	s.writeGate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.CheckForUpload(ctx, "a", upload("grant-a", good)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked policy read allowed upload")
		}
	case <-time.After(time.Second):
		<-s.writeGate
		t.Fatal("policy read ignored cancellation")
	}
	<-s.writeGate
}

func TestCachedDecisionRechecksSettingsAfterValidation(t *testing.T) {
	s, _, _ := newFixture(t)
	if _, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err != nil {
		t.Fatal(err)
	}
	var validates int
	probe := upload("grant-a", good)
	probe.Validate = func() error {
		validates++
		if validates == 2 {
			c, err := s.Settings()
			if err != nil {
				return err
			}
			c.ReasoningEffort = "medium"
			_, err = s.Update(context.Background(), c)
			return err
		}
		return nil
	}
	d, err := s.CheckForUpload(context.Background(), "a", probe)
	if err == nil || d.Allowed {
		t.Fatal("cache used after configuration changed inside validation")
	}
}

func TestSettingsRejectSymlinksAndOpenPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Unix permissions and symlink behavior")
	}
	s, _, root := newFixture(t)
	path := filepath.Join(root, directoryName, settingsName)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSettings(root); !errors.Is(err, ErrSettingsUnavailable) {
		t.Fatal("public-readable settings accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "policy-target.json")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSettings(root); !errors.Is(err, ErrSettingsUnavailable) {
		t.Fatal("symlink policy accepted")
	}
	if _, err := s.Settings(); err == nil {
		t.Fatal("live service trusted symlink")
	}
}

func TestPurgeFailurePreservesRetryableHistory(t *testing.T) {
	s, f, root := newFixture(t)
	if _, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.accounts = nil
	f.mu.Unlock()
	if err := s.purgeAccountIDs(context.Background(), []string{"a"}, func(string) error { return errors.New("synthetic remove failure") }); !errors.Is(err, ErrCacheCleanup) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, directoryName, snapshotFile("a"))); err != nil {
		t.Fatal("failed cleanup discarded cache")
	}
	if err := s.PurgeAccountIDs(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
}

func TestCooldownRejectsNewAttemptWithoutUsingOldNormal(t *testing.T) {
	s, f, _ := newFixture(t)
	c, _ := s.Settings()
	c.CooldownSeconds = 60
	if _, err := s.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckForUpload(context.Background(), "a", upload("grant-a", good)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Refresh(context.Background(), []string{"a"}, false)
	if err != nil || len(r.Accepted) != 0 || len(r.Skipped) != 1 {
		t.Fatalf("cooldown ignored %#v %v", r, err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("cooling manual probe acquired grant")
	}
	d, err := s.CheckForUpload(context.Background(), "a", upload("different-grant", good))
	if err == nil || d.Allowed || d.ErrorCode != "cooldown" {
		t.Fatalf("new identity reused old result %#v %v", d, err)
	}
}

func TestCloseWaitsAdmittedSettingsWrite(t *testing.T) {
	s, _, _ := newFixture(t)
	write := s.persist
	started, release := make(chan struct{}), make(chan struct{})
	s.persist = func(ctx context.Context, name string, value any) error {
		if name == settingsName {
			close(started)
			<-release
		}
		return write(ctx, name, value)
	}
	c, _ := s.Settings()
	c.TimeoutSeconds = 91
	updated := make(chan error, 1)
	go func() { _, err := s.Update(context.Background(), c); updated <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close passed pending durable write: %v", err)
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsContextCancelsWaitWithoutInterruptingAdmittedWrite(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := newFixture(t)
			settings, err := s.Settings()
			if err != nil {
				t.Fatal(err)
			}
			write := s.persist
			started, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			s.persist = func(ctx context.Context, name string, value any) error {
				if name == settingsName {
					close(started)
					<-release
				}
				return write(ctx, name, value)
			}
			settings.TimeoutSeconds = 91
			updated := make(chan error, 1)
			go func() { _, err := s.Update(context.Background(), settings); updated <- err }()
			<-started
			ctx, cancel := context.WithCancel(t.Context())
			want := context.Canceled
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			read := make(chan error, 1)
			go func() {
				value, err := s.SettingsContext(ctx)
				if value != (Settings{}) {
					read <- errors.New("cancelled read exposed an unconfirmed policy")
					return
				}
				read <- err
			}()
			if mode == "cancel" {
				select {
				case err := <-read:
					t.Fatal("read bypassed the admitted writer", err)
				case <-time.After(10 * time.Millisecond):
				}
				cancel()
			}
			select {
			case err := <-read:
				if !errors.Is(err, want) {
					t.Fatalf("policy read ignored cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("policy read stayed blocked after its caller cancelled")
			}
			select {
			case err := <-updated:
				t.Fatal("read cancellation claimed to finish the independent writer", err)
			default:
			}
			close(release)
			released = true
			if err := <-updated; err != nil {
				t.Fatal("read cancellation affected the writer", err)
			}
			if saved, err := s.SettingsContext(nil); err != nil || saved.TimeoutSeconds != 91 {
				t.Fatal("writer did not retain its requested policy", saved, err)
			}
			if _, err := s.SettingsContext(ctx); !errors.Is(err, want) {
				t.Fatal("already-cancelled read acquired the now-free gate", err)
			}
		})
	}
}
