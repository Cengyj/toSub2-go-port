package jobs

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"
	"tosub2-go-port/internal/store"
)

func scaleFixture(b *testing.B, count, lines int, disk bool) *Manager {
	b.Helper()
	m := NewManager(context.Background(), 1, nil, nil)
	b.Cleanup(m.cancel)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("scale-%04d", i)
		j := &Job{ID: id, Email: fmt.Sprintf("scale-%04d@example.test", i), Status: StatusImported, CreatedAt: at.Add(time.Duration(i) * time.Second), UpdatedAt: at, Input: map[string]any{"password": "Synthetic-password"}, HasPassword: true}
		for n := 0; n < lines; n++ {
			entry := LogEntry{At: at.Add(time.Duration(n) * time.Second), Stage: "synthetic", Status: "running", Message: "Synthetic operation state", Level: "info"}
			j.LogEntries = append(j.LogEntries, entry)
			j.Logs = append(j.Logs, fmt.Sprintf("%s [info] %s", entry.At.Format(time.RFC3339), entry.Message))
		}
		m.jobs[id] = j
	}
	if disk {
		var err error
		m.store, err = store.New(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
	}
	return m
}

func reportQuantiles(b *testing.B, samples []time.Duration) {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) > 0 {
		b.ReportMetric(float64(samples[len(samples)/2].Nanoseconds()), "p50-ns")
		b.ReportMetric(float64(samples[(len(samples)-1)*95/100].Nanoseconds()), "p95-ns")
	}
}
func BenchmarkScalePublicAccounts(b *testing.B) {
	for _, count := range []int{100, 1000} {
		for _, lines := range []int{0, 20} {
			b.Run(fmt.Sprintf("accounts-%d/logs-%d", count, lines), func(b *testing.B) {
				m := scaleFixture(b, count, lines, false)
				samples := make([]time.Duration, 0, b.N)
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					start := time.Now()
					_ = m.PublicAccounts()
					samples = append(samples, time.Since(start))
				}
				b.StopTimer()
				reportQuantiles(b, samples)
			})
		}
	}
}

func BenchmarkScaleFilteredAccounts(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("accounts-%d/matched-2", count), func(b *testing.B) {
			m := scaleFixture(b, count, 20, false)
			emails := []string{"scale-0000@example.test", "scale-0001@example.test"}
			samples := make([]time.Duration, 0, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				start := time.Now()
				got := m.PublicAccountsFiltered(emails)
				if got.Total != count || len(got.Accounts) != 2 {
					b.Fatal("filter fixture returned an inconsistent account set")
				}
				samples = append(samples, time.Since(start))
			}
			b.StopTimer()
			reportQuantiles(b, samples)
		})
	}
}

func BenchmarkScaleQueryAccounts(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("accounts-%d", count), func(b *testing.B) {
			m := scaleFixture(b, count, 20, false)
			samples := make([]time.Duration, 0, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				start := time.Now()
				_ = m.QueryAccounts()
				samples = append(samples, time.Since(start))
			}
			b.StopTimer()
			reportQuantiles(b, samples)
		})
	}
}
func BenchmarkScaleMetadataCommit(b *testing.B) {
	for _, count := range []int{100, 1000} {
		for _, lines := range []int{0, 20} {
			b.Run(fmt.Sprintf("accounts-%d/logs-%d", count, lines), func(b *testing.B) {
				m := scaleFixture(b, count, lines, true)
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				coldStart := time.Now()
				m.persistLocked()
				coldDuration := time.Since(coldStart)
				if m.lastPersistError != nil {
					b.Fatal(m.lastPersistError)
				}
				runtime.GC()
				runtime.GC()
				runtime.ReadMemStats(&after)
				retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
				info, err := os.Stat(m.store.Path("job-meta.json"))
				if err != nil {
					b.Fatal(err)
				}
				samples := make([]time.Duration, 0, b.N)
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					start := time.Now()
					m.mu.Lock()
					m.jobs["scale-0000"].UpdatedAt = time.Now().UTC()
					m.persistLocked()
					err := m.lastPersistError
					m.mu.Unlock()
					if err != nil {
						b.Fatal(err)
					}
					samples = append(samples, time.Since(start))
				}
				b.StopTimer()
				reportQuantiles(b, samples)
				b.ReportMetric(float64(coldDuration.Nanoseconds()), "cold-ns")
				b.ReportMetric(float64(retained), "retained-B")
				b.ReportMetric(float64(info.Size()), "document-B")
				b.ReportMetric(1, "commits/op")
				runtime.KeepAlive(m)
			})
		}
	}
}
