package browser

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func occupiedPool(t *testing.T) (*Manager, *Session) {
	t.Helper()
	m := NewManager(filepath.Join(t.TempDir(), "missing-chrome"), 1)
	s := &Session{owner: m, Dir: t.TempDir()}
	m.active[s] = struct{}{}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m, s
}

func TestBrowserPoolWaitCancellation(t *testing.T) {
	m, _ := occupiedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.StartWait(ctx, "", "", "", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capacity must wait until cancellation: %v", err)
	}
}

func TestBrowserPoolReleaseWakesWaiter(t *testing.T) {
	m, s := occupiedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.StartWait(ctx, "", "", "", ""); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("waiting task failed while pool was full: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		// The nonexistent executable proves the waiter progressed to startup.
		if err == nil || errors.Is(err, ErrCapacity) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter did not acquire released capacity: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("capacity release did not wake waiter")
	}
}

func TestBrowserPoolShutdownWakesWaiter(t *testing.T) {
	m, _ := occupiedPool(t)
	done := make(chan error, 1)
	go func() { _, err := m.StartWait(context.Background(), "", "", "", ""); done <- err }()
	m.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("unexpected close result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left a browser waiter blocked")
	}
}
