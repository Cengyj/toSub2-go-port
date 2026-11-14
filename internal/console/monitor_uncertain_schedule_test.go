package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/sub2api"
)

func TestMonitorUncertainEnableWakesExistingSchedule(t *testing.T) {
	t.Setenv("SUB2API_MONITOR_INTERVAL_MS", "3600000")
	srv, _ := newMonitorTestServer(t)
	var reads, writes atomic.Int32
	checked := make(chan struct{}, 8)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/api/v1/admin/accounts" || r.Header.Get("x-api-key") != "fixture-key" {
			t.Error("unexpected monitor request")
		}
		reads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
		checked <- struct{}{}
	}))
	defer remote.Close()
	cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-key"}
	if err := srv.configureMonitor(cfg, true, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.monitorWake:
	default:
	}
	srv.StartMonitor(t.Context())
	waitCheck := func() {
		t.Helper()
		select {
		case <-checked:
		case <-time.After(time.Second):
			t.Fatal("configuration did not wake monitor before its previous one-hour interval")
		}
	}
	waitDeadline := func() {
		t.Helper()
		deadline := time.After(time.Second)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			srv.monitorMu.Lock()
			next := srv.monitorNextCheckAt
			srv.monitorMu.Unlock()
			if next != nil {
				return
			}
			select {
			case <-deadline:
				t.Fatal("monitor did not publish its armed schedule")
			case <-ticker.C:
			}
		}
	}
	waitCheck()
	waitDeadline()
	// Put the already-armed scheduler into the disabled policy without a wake.
	// Unlike a sleep, its observed initial deadline proves the old timer exists.
	disabled := srv.monitor.Configuration()
	disabled.Enabled = false
	if err := srv.monitor.Configure(sub2api.New(cfg), disabled, srv.monitorHooks(cfg)); err != nil {
		t.Fatal(err)
	}
	if state := srv.publicMonitorState(); state["enabled"] != false || state["nextCheckAt"].(*time.Time) != nil {
		t.Fatal("fixture did not disable monitor")
	}
	var configurations atomic.Int32
	srv.monitorConfigure = func(client *sub2api.Client, next sub2api.MonitorConfig, hooks sub2api.MonitorHooks) error {
		configurations.Add(1)
		if err := srv.monitor.Configure(client, next, hooks); err != nil {
			return err
		}
		// The replacement is visible; simulate failure of its final durability
		// confirmation without retrying Configure or any remote operation.
		return privatefile.ErrDurabilityUncertain
	}
	err := srv.configureMonitorContext(context.Background(), cfg, true, false)
	if !errors.Is(err, privatefile.ErrDurabilityUncertain) || !srv.monitor.Configuration().Enabled {
		t.Fatal("uncertain visible policy lost its error or enabled state", err)
	}
	waitCheck()
	waitDeadline()
	srv.Close()
	if configurations.Load() != 1 || reads.Load() != 2 || writes.Load() != 0 {
		t.Fatalf("configuration/network replay: configuration=%d reads=%d writes=%d", configurations.Load(), reads.Load(), writes.Load())
	}
}
