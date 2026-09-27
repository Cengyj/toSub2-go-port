package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

// The real scheduler permits twenty account runners while the browser pool
// admits four. Waiting for manual input retains the account's browser; waiting
// for capacity must remain cancelable and must not exhaust a request timeout.
func TestChromiumPoolTwentyJobsManualInputAndQueuedCancellation(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real twenty-job/four-browser pool integration")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("Chrome not installed; set CHROME_PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := NewManager(path, 4)
	m := jobs.NewManager(ctx, 20, nil, nil)
	t.Cleanup(func() {
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := m.Shutdown(stop); err != nil {
			t.Error(err)
		}
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	ready := make(chan string, 21)
	var entered atomic.Int32
	m.SetRunner(func(task context.Context, job *jobs.Job, emit func(jobs.JobEvent)) (result error) {
		entered.Add(1)
		emit(jobs.JobEvent{Status: jobs.StatusRunning, Stage: "browser_wait"})
		s, err := pool.StartWait(task, "", "", "", "device-"+job.ID)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, pool.Stop(s)) }()
		emit(jobs.JobEvent{Status: jobs.StatusWaitingInput, Stage: "email_otp", Prompt: "fixture manual input"})
		ready <- job.ID
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, ok := m.ConsumeInput(job.ID, "email_otp"); ok {
				_, err := s.Evaluate(task, "1")
				return err
			}
			select {
			case <-task.Done():
				return task.Err()
			case <-ticker.C:
			}
		}
	})
	for n := 0; n < 21; n++ {
		if _, err := m.Submit(jobs.Spec{ID: fmt.Sprintf("pool-%02d", n), Email: fmt.Sprintf("pool-%02d@example.invalid", n)}); err != nil {
			t.Fatal(err)
		}
	}
	m.Start()
	var active []string
	for n := 0; n < 4; n++ {
		select {
		case id := <-ready:
			active = append(active, id)
		case <-ctx.Done():
			t.Fatal("initial four browsers did not become ready")
		}
	}
	// Exceed a short operation budget while the sixteen account runners wait;
	// only their long-lived job context controls capacity/manual-input waiting.
	time.Sleep(150 * time.Millisecond)
	if entered.Load() != 20 {
		t.Fatalf("scheduler started %d runners, want 20", entered.Load())
	}
	waiting, queued, waitingForBrowser := 0, 0, 0
	for _, job := range m.List() {
		switch job.Status {
		case "email_otp":
			waiting++
		case jobs.StatusQueued:
			queued++
		case "working":
			waitingForBrowser++
		default:
			t.Fatalf("job failed while waiting for browser/input: %s", job.Status)
		}
	}
	if waiting != 4 || queued != 1 || waitingForBrowser != 16 {
		t.Fatalf("pool/scheduler limits drifted: input=%d queued=%d browser_wait=%d", waiting, queued, waitingForBrowser)
	}
	pool.mu.Lock()
	count := len(pool.active) + pool.starting
	pool.mu.Unlock()
	if count != 4 {
		t.Fatalf("browser limit drifted: %d", count)
	}
	select {
	case <-ready:
		t.Fatal("more than four browser sessions admitted")
	default:
	}
	if !m.Input(active[0], map[string]any{"email_otp": "fixture-only"}) {
		t.Fatal("manual input rejected")
	}
	var admitted string
	select {
	case admitted = <-ready:
	case <-ctx.Done():
		t.Fatal("manual completion did not release browser capacity")
	}
	for _, prior := range active {
		if admitted == prior {
			t.Fatal("released capacity did not admit a waiting job")
		}
	}
	pool.mu.Lock()
	count = len(pool.active) + pool.starting
	pool.mu.Unlock()
	if count > 4 {
		t.Fatalf("capacity release exceeded browser limit: %d", count)
	}
	m.CancelAll()
	deadline := time.Now().Add(5 * time.Second)
	for {
		finished := true
		for _, job := range m.List() {
			if job.Status == jobs.StatusRunning || job.Status == "working" || job.Status == "email_otp" || job.Status == jobs.StatusQueued {
				finished = false
			}
		}
		if finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel-all left queued browser/input jobs blocked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, job := range m.List() {
		want := jobs.StatusCanceled
		if job.ID == active[0] {
			want = jobs.StatusSucceeded
		}
		if job.Status != want {
			t.Fatalf("job %s status=%s want=%s", job.ID, job.Status, want)
		}
	}
	pool.mu.Lock()
	count = len(pool.active) + pool.starting
	pool.mu.Unlock()
	if count != 0 {
		t.Fatalf("cancel-all retained %d browser slots", count)
	}
}
