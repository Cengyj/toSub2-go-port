package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/recyclebin"
)

type shutdownTrashRepository struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *shutdownTrashRepository) ListTrash() []*jobs.TrashEntry {
	return []*jobs.TrashEntry{{ID: "synthetic-purge", State: jobs.TrashStatePurging}}
}
func (r *shutdownTrashRepository) PurgeTrash(context.Context, []string) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return 1, nil
}

func TestConsoleCloseCancelsOtherWorkersBeforeWaitingForTrash(t *testing.T) {
	repo := &shutdownTrashRepository{entered: make(chan struct{}), release: make(chan struct{})}
	retention, err := recyclebin.New(t.TempDir(), repo)
	if err != nil {
		t.Fatal(err)
	}
	smsCtx, smsCancel := context.WithCancel(context.Background())
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	monitorDone := make(chan struct{})
	s := &Server{recycleBin: retention, smsCtx: smsCtx, smsStop: smsCancel, monitorCancel: monitorCancel, monitorDone: monitorDone}
	defer smsCancel()
	defer monitorCancel()
	go func() { <-monitorCtx.Done(); close(monitorDone) }()
	retention.Start(t.Context())
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("synthetic cleanup did not start")
	}
	defer close(repo.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- s.CloseContext(ctx) }()
	for name, worker := range map[string]context.Context{"SMS": smsCtx, "monitor": monitorCtx} {
		select {
		case <-worker.Done():
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("%s cancellation waited for unrelated cleanup", name)
		}
	}
	select {
	case err := <-closed:
		t.Fatalf("shutdown returned before admitted cleanup completed: %v", err)
	default:
	}
	cancel()
	if err := <-closed; !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown lost its caller cancellation", err)
	}
}

func TestConsoleCloseRetainsMonitorOwnershipAfterTimeout(t *testing.T) {
	smsCtx, smsCancel := context.WithCancel(context.Background())
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := &Server{smsCtx: smsCtx, smsStop: smsCancel, monitorCancel: monitorCancel, monitorDone: done}
	defer smsCancel()
	defer monitorCancel()
	defer close(done)
	for attempt := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := s.CloseContext(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d forgot a monitor that still owns work: %v", attempt+1, err)
		}
	}
	if monitorCtx.Err() == nil {
		t.Fatal("monitor was not canceled")
	}
}

func TestConsoleClosePreservesEveryIncompleteComponent(t *testing.T) {
	smsCtx, smsCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := &Server{smsCtx: smsCtx, smsStop: smsCancel, monitorDone: done}
	s.smsWG.Add(1)
	defer s.smsWG.Done()
	defer smsCancel()
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := s.CloseContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "SMS shutdown") || !strings.Contains(err.Error(), "monitor shutdown") {
		t.Fatalf("shutdown omitted an incomplete component: %v", err)
	}
}

func TestConsoleClosePreventsMonitorRestart(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	if err := s.CloseContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.StartMonitor(t.Context())
	s.monitorMu.Lock()
	started := s.monitorCancel != nil || s.monitorDone != nil
	s.monitorMu.Unlock()
	if started {
		t.Fatal("closed console started a new monitor worker")
	}
}
