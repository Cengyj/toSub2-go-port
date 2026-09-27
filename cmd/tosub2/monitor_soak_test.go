package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// This uses the unchanged five-minute production interval, not a shorter test
// clock. The second remote request deliberately remains pending until service
// shutdown, proving that shutdown cancels in-flight monitoring as well.
func TestConsoleDefaultFiveMinuteMonitorAndInflightShutdown(t *testing.T) {
	if os.Getenv("TOSUB2_LONG_MONITOR_TEST") != "1" {
		t.Skip("set TOSUB2_LONG_MONITOR_TEST=1 for the actual five-minute monitor lifecycle")
	}
	for _, key := range []string{"SUB2API_MONITOR_INTERVAL_MS", "SUB2API_AUTO_REPAIR_COOLDOWN_MS"} {
		t.Setenv(key, "")
	}
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", t.TempDir())
	requests := make(chan time.Time, 4)
	canceled := make(chan struct{}, 1)
	var count atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/admin/accounts" || r.Header.Get("x-api-key") != "synthetic-soak-admin-key" || r.URL.Query().Get("status") != "error" {
			t.Error("monitor request did not match the authenticated error-account contract")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		n := count.Add(1)
		select {
		case requests <- time.Now():
		default:
		}
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "{\"data\":{\"items\":[],\"pages\":1}}")
			return
		}
		<-r.Context().Done()
		select {
		case canceled <- struct{}{}:
		default:
		}
	}))
	defer remote.Close()
	t.Setenv("SUB2API_BASE_URL", remote.URL)
	t.Setenv("SUB2API_ADMIN_API_KEY", "synthetic-soak-admin-key")
	cfg := lifecycleConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- runConsole(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("console could not start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("console startup timeout")
	}
	var first time.Time
	select {
	case first = <-requests:
	case <-time.After(10 * time.Second):
		t.Fatal("monitor did not perform its startup check")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.Addr()+"/api/sub2api/monitor", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Console-Token", cfg.StartupToken)
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	err = json.NewDecoder(response.Body).Decode(&state)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || state["intervalMinutes"] != float64(5) {
		t.Fatal("default monitor interval is not exposed as five minutes")
	}
	select {
	case second := <-requests:
		interval := second.Sub(first)
		if interval < 5*time.Minute || interval > 5*time.Minute+30*time.Second {
			t.Fatalf("default monitor repeated after %v, expected five minutes", interval)
		}
		t.Logf("observed production monitor interval: %v", interval)
	case <-ctx.Done():
		t.Fatal("default five-minute monitor did not repeat")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown left the monitor HTTP request running")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor shutdown failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("console did not finish shutdown")
	}
	if count.Load() != 2 {
		t.Fatal("monitor started an unexpected overlapping request")
	}
	metadata, err := os.ReadFile(filepath.Join(cfg.OutputRoot, "job-meta.json"))
	if err != nil || !json.Valid(metadata) {
		t.Fatal("console did not save valid metadata during monitor shutdown")
	}
}
