package accountquality

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

func TestCompletionGateTimeoutFinishesAttemptWithoutLosingHistory(t *testing.T) {
	for _, tc := range []struct {
		name, status, code string
		cause              error
	}{
		{"success", "failed", "persistence_failed", nil},
		{"cancelled", "cancelled", "cancelled", context.Canceled},
		{"timeout", "failed", "timeout", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testCompletionGateTimeout(t, tc.status, tc.code, tc.cause)
		})
	}
}

func testCompletionGateTimeout(t *testing.T, wantStatus, wantCode string, cause error) {
	t.Helper()
	s, fixture, root := newFixture(t)
	if _, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", good)); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshots([]string{"a"})
	enable(t, s, func(c *Settings) { c.CooldownSeconds = 10 })
	started, release := make(chan struct{}), make(chan struct{})
	fixture.mu.Lock()
	fixture.probe = func(ctx context.Context, o modelprobe.Options) (modelprobe.Result, error) {
		close(started)
		select {
		case <-release:
			if cause != nil {
				return modelprobe.Result{}, cause
			}
			return good(ctx, o)
		case <-ctx.Done():
			return modelprobe.Result{}, ctx.Err()
		}
	}
	fixture.mu.Unlock()
	if receipt, err := s.Refresh(t.Context(), []string{"a"}, false); err != nil || len(receipt.Accepted) != 1 {
		t.Fatalf("refresh: %+v, %v", receipt, err)
	}
	<-started
	s.mu.Lock()
	f := s.flights["email-a"]
	s.mu.Unlock()
	// A concurrent policy/cache writer may hold the gate beyond the existing
	// three-second completion budget. Exercise that real budget unchanged.
	if err := s.lockWrite(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.unlockWrite()
	close(release)
	select {
	case <-f.done:
	case <-time.After(6 * time.Second):
		t.Fatal("completion did not honor its bounded cleanup budget")
	}
	after, _ := s.Snapshots([]string{"a"})
	snapshot := after["a"]
	if !errors.Is(f.err, ErrCachePersistence) || cause != nil && !errors.Is(f.err, cause) || snapshot.LastAttempt.Status != wantStatus || snapshot.LastAttempt.ErrorCode != wantCode || snapshot.LastAttempt.FinishedAt == nil || snapshot.Persisted {
		t.Fatalf("finished probe still advertised a live or persisted attempt: %+v; persisted=%v, error=%v", snapshot.LastAttempt, snapshot.Persisted, f.err)
	}
	if !reflect.DeepEqual(snapshot.LastResult, before["a"].LastResult) || !reflect.DeepEqual(snapshot.ProbeHistory, before["a"].ProbeHistory) {
		t.Fatal("uncommitted completion replaced the last confirmed history")
	}
	if snapshot.State != "coolingDown" || fixture.calls.Load() != 1 || fixture.released.Load() != 1 || s.Status().Active != 0 {
		t.Fatal("completion lost cooldown or failed to release its account lease")
	}
	var saved storedRow
	if err := readPrivateJSON(s.disk.Root, snapshotFile("a"), &saved); err != nil || saved.Snapshot.LastAttempt.Status != "running" {
		t.Fatal("completion erased the durable attempt barrier", err)
	}
	restored, err := New(root, fixtureProvider(fixture))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := restored.Snapshots([]string{"a"})
	if rows["a"].LastAttempt.Status != "cancelled" || fixture.calls.Load() != 1 {
		t.Fatal("restart reused success or replayed an uncertain observation")
	}
}

func TestFailedProbeCannotAuthorizeUploadWithoutDurableCompletion(t *testing.T) {
	for _, mode := range []string{"write-failure", "gate-timeout"} {
		for _, code := range []string{"timeout", "network_error"} {
			t.Run(mode+"/"+code, func(t *testing.T) {
				t.Parallel()
				s, _, _ := newFixture(t)
				enable(t, s, func(c *Settings) { c.BlockFailed = false })
				if mode == "write-failure" {
					write := s.persist
					s.persist = func(ctx context.Context, name string, value any) error {
						if row, ok := value.(storedRow); ok && row.Snapshot.LastAttempt.Status != "running" {
							return errors.New("synthetic completion write failure")
						}
						return write(ctx, name, value)
					}
				}
				started, release := make(chan struct{}), make(chan struct{})
				type outcome struct {
					decision UploadDecision
					err      error
				}
				done := make(chan outcome, 1)
				go func() {
					d, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
						close(started)
						<-release
						return modelprobe.Result{}, &modelprobe.Error{Code: code}
					}))
					done <- outcome{d, err}
				}()
				<-started
				s.mu.Lock()
				f := s.flights["email-a"]
				s.mu.Unlock()
				if mode == "gate-timeout" {
					if err := s.lockWrite(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				if mode == "gate-timeout" {
					select {
					case <-f.done:
						s.unlockWrite()
					case <-time.After(6 * time.Second):
						s.unlockWrite()
						t.Fatal("completion exceeded its cleanup budget")
					}
				}
				result := <-done
				if result.decision.Allowed || !errors.Is(result.err, ErrCachePersistence) || result.decision.ErrorCode != "persistence_failed" {
					t.Fatalf("permissive probe policy bypassed uncommitted evidence: %+v, %v", result.decision, result.err)
				}
				rows, _ := s.Snapshots([]string{"a"})
				if rows["a"].LastAttempt.ErrorCode != code || rows["a"].Persisted {
					t.Fatal("storage failure erased the observed probe failure or claimed durability")
				}
			})
		}
	}
}
