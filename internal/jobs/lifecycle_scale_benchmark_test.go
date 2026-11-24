package jobs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

func benchmarkDurations(b *testing.B, name string, values []time.Duration) {
	b.Helper()
	if len(values) == 0 {
		b.Fatal("missing timing samples", name)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	b.ReportMetric(float64(values[len(values)/2].Nanoseconds()), name+"-p50-ns")
	b.ReportMetric(float64(values[(len(values)*95+99)/100-1].Nanoseconds()), name+"-p95-ns")
	b.ReportMetric(float64(values[len(values)-1].Nanoseconds()), name+"-max-ns")
}

// Two operations per synthetic email exercise the queue's head-of-line skip
// as well as its ordinary FIFO admission. Work is one 200us local timer; no
// network, credentials or disks are involved. Timings describe this fixture,
// not upstream login latency. The second scenario cancels occupied slots.
func BenchmarkSchedulerScaleLifecycle(b *testing.B) {
	for _, count := range []int{100, 1000} {
		for _, concurrency := range []int{1, 20} {
			for _, stopActive := range []bool{false, true} {
				b.Run(fmt.Sprintf("accounts-%d/concurrency-%d/stop-active-%t", count, concurrency, stopActive), func(b *testing.B) {
					var queueWait, shutdown []time.Duration
					var current *Manager
					b.Cleanup(func() {
						if current != nil {
							current.cancel()
						}
					})
					b.ReportAllocs()
					for n := 0; n < b.N; n++ {
						b.StopTimer()
						m := NewManager(b.Context(), concurrency, nil, nil)
						current = m
						var statsMu sync.Mutex
						active := map[string]int{}
						seen := map[string]bool{}
						var overlap, duplicate bool
						started := make(chan struct{}, count*2)
						m.SetRunner(func(ctx context.Context, job *Job, _ func(JobEvent)) error {
							statsMu.Lock()
							active[job.Email]++
							overlap = overlap || active[job.Email] > 1
							duplicate = duplicate || seen[job.ID]
							seen[job.ID] = true
							queueWait = append(queueWait, job.StartedAt.Sub(job.QueuedAt))
							statsMu.Unlock()
							started <- struct{}{}
							defer func() { statsMu.Lock(); active[job.Email]--; statsMu.Unlock() }()
							if stopActive {
								<-ctx.Done()
								return ctx.Err()
							}
							timer := time.NewTimer(200 * time.Microsecond)
							defer timer.Stop()
							select {
							case <-ctx.Done():
								return ctx.Err()
							case <-timer.C:
								return nil
							}
						})
						for i := 0; i < count; i++ {
							for attempt := 0; attempt < 2; attempt++ {
								if _, err := m.Submit(Spec{ID: fmt.Sprintf("lifecycle-%04d-%d", i, attempt), Email: fmt.Sprintf("lifecycle-%04d@example.test", i)}); err != nil {
									b.Fatal(err)
								}
							}
						}
						deadline, cancel := context.WithTimeout(b.Context(), 10*time.Second)
						b.StartTimer()
						m.Start()
						wanted := count * 2
						if stopActive {
							wanted = concurrency
						}
						for i := 0; i < wanted; i++ {
							select {
							case <-started:
							case <-deadline.Done():
								m.cancel()
								cancel()
								b.Fatal("scheduler starved an eligible synthetic operation")
							}
						}
						if !stopActive {
							poll := time.NewTicker(100 * time.Microsecond)
							for !m.ActivitySnapshot().Drained {
								select {
								case <-poll.C:
								case <-deadline.Done():
									poll.Stop()
									m.cancel()
									cancel()
									b.Fatal("scheduler did not finish owned operations")
								}
							}
							poll.Stop()
						}
						closing := time.Now()
						if err := m.Shutdown(deadline); err != nil {
							cancel()
							b.Fatal(err)
						}
						shutdown = append(shutdown, time.Since(closing))
						b.StopTimer()
						cancel()
						current = nil
						if overlap || duplicate || len(seen) != wanted || len(m.active) != 0 {
							b.Fatal("scheduler overlapped an email, replayed, starved or retained owned work")
						}
						if !stopActive {
							for _, job := range m.jobs {
								if job.Status != StatusSucceeded {
									b.Fatal("synthetic operation did not finish successfully")
								}
							}
						}
					}
					benchmarkDurations(b, "queue", queueWait)
					benchmarkDurations(b, "shutdown", shutdown)
					b.ReportMetric(0, "same-email-overlaps")
				})
			}
		}
	}
}

// This is an explicit contention scenario: readers reach the manager mutex
// while one warm metadata commit owns it. Report actual wait, including lock
// handoff, without claiming it represents a production arrival distribution.
func BenchmarkMetadataCommitLockWait(b *testing.B) {
	for _, count := range []int{100, 1000} {
		for _, readers := range []int{1, 20} {
			b.Run(fmt.Sprintf("accounts-%d/readers-%d", count, readers), func(b *testing.B) {
				m := scaleFixture(b, count, 20, true)
				m.persistLocked()
				if m.lastPersistError != nil {
					b.Fatal(m.lastPersistError)
				}
				var waits []time.Duration
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					ready := make(chan struct{}, readers)
					done := make(chan time.Duration, readers)
					m.mu.Lock()
					for i := 0; i < readers; i++ {
						go func() {
							start := time.Now()
							ready <- struct{}{}
							m.mu.Lock()
							elapsed := time.Since(start)
							m.mu.Unlock()
							done <- elapsed
						}()
					}
					for i := 0; i < readers; i++ {
						<-ready
					}
					m.jobs["scale-0000"].UpdatedAt = time.Now().UTC()
					m.persistLocked()
					err := m.lastPersistError
					m.mu.Unlock()
					for i := 0; i < readers; i++ {
						waits = append(waits, <-done)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				benchmarkDurations(b, "lock-wait", waits)
				b.ReportMetric(1, "commits/op")
			})
		}
	}
}
