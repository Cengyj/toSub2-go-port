package console

import (
	"context"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

// BenchmarkConsoleAccountPage includes authorization, projection, pagination,
// cross-page selection, encoding and local discovery. All storage is synthetic;
// no scheduler or upstream client starts. Run with -benchtime=30x -benchmem.
func BenchmarkConsoleAccountPage(b *testing.B) {
	benchmarkConsoleAccountPages(b, false)
}

// The bundled UI opts into a typed selection index. Keep the legacy response
// in the same benchmark so transport savings cannot conceal a default-path
// regression; fixture, ordering, page size and request count remain identical.
func BenchmarkConsoleCompactAccountPage(b *testing.B) {
	benchmarkConsoleAccountPages(b, true)
}

func benchmarkConsoleAccountPages(b *testing.B, compact bool) {
	for _, count := range []int{100, 1000} {
		for _, filtered := range []bool{false, true} {
			b.Run(fmt.Sprintf("accounts-%d/filter-%t", count, filtered), func(b *testing.B) {
				root := b.TempDir()
				disk, err := store.New(root)
				if err != nil {
					b.Fatal(err)
				}
				rows := make([]map[string]any, 0, count)
				at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				for n := 0; n < count; n++ {
					logs := make([]string, 20)
					for i := range logs {
						logs[i] = "Synthetic completed operation; no private data"
					}
					rows = append(rows, map[string]any{"id": fmt.Sprintf("scale-%04d", n), "email": fmt.Sprintf("scale-%04d@example.test", n), "created_at": at.Add(time.Duration(n) * time.Second), "updated_at": at, "status": "imported", "authorization_method": "chatgpt", "logs": logs})
				}
				if err := disk.WriteJSON(b.Context(), "job-meta.json", map[string]any{"version": 2, "jobs": rows, "trash": []any{}}); err != nil {
					b.Fatal(err)
				}
				manager := jobs.NewManager(b.Context(), 20, disk, slog.New(slog.NewTextHandler(io.Discard, nil)))
				vault := credentials.NewMemoryStore()
				manager.SetCredentialStore(vault)
				if err := manager.Restore(b.Context()); err != nil {
					b.Fatal(err)
				}
				server := NewServer(config.Config{OutputRoot: root, StartupToken: "synthetic-token"}, manager, embed.FS{}, nil, vault)
				b.Cleanup(func() {
					server.Close()
					if err := manager.Shutdown(context.Background()); err != nil {
						b.Error(err)
					}
				})
				request := func() *http.Request {
					r := httptest.NewRequest(http.MethodGet, "/api/jobs?page=1&pageSize=20", nil)
					if filtered {
						r = httptest.NewRequest(http.MethodPost, "/api/jobs/query", strings.NewReader(`{"emails":["scale-0000@example.test","scale-0001@example.test"],"page":1,"pageSize":20}`))
					}
					if compact {
						query := r.URL.Query()
						query.Set("selection", "compact")
						r.URL.RawQuery = query.Encode()
					}
					r.Host = "127.0.0.1"
					r.Header.Set("X-Console-Token", "synthetic-token")
					return r
				}
				// Warm the throttled local discovery and projection allocations once.
				server.Handler().ServeHTTP(httptest.NewRecorder(), request())
				samples := make([]time.Duration, 0, b.N)
				var responseBytes int
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					start := time.Now()
					w := httptest.NewRecorder()
					server.Handler().ServeHTTP(w, request())
					if w.Code != http.StatusOK {
						b.Fatal("synthetic page failed", w.Code)
					}
					samples = append(samples, time.Since(start))
					responseBytes = w.Body.Len()
				}
				b.StopTimer()
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				if len(samples) > 0 {
					b.ReportMetric(float64(samples[len(samples)/2].Nanoseconds()), "p50-ns")
					b.ReportMetric(float64(samples[(len(samples)-1)*95/100].Nanoseconds()), "p95-ns")
				}
				b.ReportMetric(float64(responseBytes), "response-B")
			})
		}
	}
}
