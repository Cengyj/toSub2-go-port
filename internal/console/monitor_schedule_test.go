package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/sub2api"
)

func TestMonitorNextCheckTracksActualTimerReconfigurationDisableAndClose(t *testing.T) {
	t.Setenv("SUB2API_MONITOR_INTERVAL_MS", "1000")
	srv, _ := newMonitorTestServer(t)
	initialEntered, initialReleased := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseInitial := func() { releaseOnce.Do(func() { close(initialReleased) }) }
	var calls atomic.Int32
	checked := make(chan time.Time, 16)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		checked <- time.Now()
		if call == 1 {
			close(initialEntered)
			select {
			case <-initialReleased:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
	}))
	defer remote.Close()
	defer releaseInitial()
	if state := monitorScheduleState(t, srv, ""); state.Next != nil || state.Enabled {
		t.Fatal("unconfigured monitor exposed an active timer")
	}
	if err := srv.configureMonitor(sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-key"}, true, false); err != nil {
		t.Fatal(err)
	}
	if state := monitorScheduleState(t, srv, ""); state.Next != nil || !state.Enabled {
		t.Fatal("configuration without a running scheduler invented a deadline")
	}
	drainMonitorConfigurationWake(srv)
	srv.StartMonitor(context.Background())
	defer srv.Close()
	select {
	case <-initialEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("initial monitor check did not start")
	}
	if state := monitorScheduleState(t, srv, ""); state.Next != nil || !state.Running {
		t.Fatal("initial check reported a timer that had not been armed")
	}
	releaseInitial()
	first := waitMonitorSchedule(t, srv, func(s monitorScheduleSnapshot) bool { return s.Next != nil })
	if remaining := time.Until(*first.Next); remaining <= 0 || remaining > 1100*time.Millisecond {
		t.Fatal("next-check response does not reflect configured one-second interval", remaining)
	}
	// Discard the initial check; the next arrival must be the published tick.
	<-checked
	select {
	case tick := <-checked:
		if tick.Before(first.Next.Add(-20 * time.Millisecond)) {
			t.Fatal("monitor fired before its published deadline")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("published monitor tick did not run")
	}
	second := waitMonitorSchedule(t, srv, func(s monitorScheduleSnapshot) bool {
		return s.Next != nil && s.Next.After(*first.Next)
	})
	if state := monitorScheduleState(t, srv, "{\"enabled\":false}"); state.Next != nil || state.Enabled {
		t.Fatal("disabling monitor did not immediately clear its next-check field")
	}
	disabledCalls := calls.Load()
	time.Sleep(1100 * time.Millisecond)
	if calls.Load() != disabledCalls || monitorScheduleState(t, srv, "").Next != nil {
		t.Fatal("disabled monitor retained a scheduled request")
	}
	if state := monitorScheduleState(t, srv, "{\"enabled\":true}"); !state.Enabled {
		t.Fatal("monitor could not be re-enabled")
	}
	select {
	case <-checked:
	case <-time.After(3 * time.Second):
		t.Fatal("re-enabling monitor did not wake its scheduler")
	}
	reenabled := waitMonitorSchedule(t, srv, func(s monitorScheduleSnapshot) bool {
		return s.Next != nil && s.Next.After(*second.Next)
	})
	monitorScheduleState(t, srv, "{\"enabled\":true,\"groupIds\":[7]}")
	select {
	case <-checked:
	case <-time.After(3 * time.Second):
		t.Fatal("reconfiguration did not wake the monitor")
	}
	reconfigured := waitMonitorSchedule(t, srv, func(s monitorScheduleSnapshot) bool {
		return s.Next != nil && s.Next.After(*reenabled.Next)
	})
	if len(reconfigured.Groups) != 1 || reconfigured.Groups[0] != 7 {
		t.Fatal("reconfigured schedule retained old monitor options")
	}
	srv.Close()
	if state := monitorScheduleState(t, srv, ""); state.Next != nil {
		t.Fatal("service close retained a future monitor deadline")
	}
	closedCalls := calls.Load()
	time.Sleep(1100 * time.Millisecond)
	if calls.Load() != closedCalls {
		t.Fatal("monitor timer survived service close")
	}
}

func TestMonitorNextCheckClearsOnParentCancellationAndSchedulerRestart(t *testing.T) {
	t.Setenv("SUB2API_MONITOR_INTERVAL_MS", "1000")
	srv, _ := newMonitorTestServer(t)
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}}})
	}))
	defer remote.Close()
	if err := srv.configureMonitor(sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-key"}, true, false); err != nil {
		t.Fatal(err)
	}
	drainMonitorConfigurationWake(srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.StartMonitor(ctx)
	defer srv.Close()
	first := waitMonitorSchedule(t, srv, func(s monitorScheduleSnapshot) bool { return s.Next != nil })
	cancel()
	waitMonitorSchedule(t, srv, func(s monitorScheduleSnapshot) bool { return s.Next == nil })
	srv.StartMonitor(context.Background())
	restarted := waitMonitorSchedule(t, srv, func(s monitorScheduleSnapshot) bool {
		return s.Next != nil && s.Next.After(*first.Next)
	})
	if restarted.Next.IsZero() || calls.Load() != 2 {
		t.Fatal("scheduler restart failed to replace canceled timer with a fresh check")
	}
	srv.stopMonitor()
	if state := monitorScheduleState(t, srv, ""); state.Next != nil {
		t.Fatal("explicit stop retained a next-check deadline")
	}
}

type monitorScheduleSnapshot struct {
	Next    *time.Time
	Enabled bool
	Running bool
	Groups  []int
}

func monitorScheduleState(t *testing.T, srv *Server, body string) monitorScheduleSnapshot {
	t.Helper()
	method := http.MethodGet
	if body != "" {
		method = http.MethodPost
	}
	request := httptest.NewRequest(method, "/api/sub2api/monitor", strings.NewReader(body))
	request.Host = "127.0.0.1"
	request.Header.Set("X-Console-Token", "test")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("monitor state returned %d: %s", response.Code, response.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	_, exists := fields["nextCheckAt"]
	if !exists {
		t.Fatal("monitor response omitted the legacy nextCheckAt field")
	}
	var state monitorScheduleSnapshot
	for _, field := range []struct {
		name   string
		target any
	}{{"nextCheckAt", &state.Next}, {"enabled", &state.Enabled}, {"running", &state.Running}, {"groupIds", &state.Groups}} {
		if err := json.Unmarshal(fields[field.name], field.target); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func waitMonitorSchedule(t *testing.T, srv *Server, matches func(monitorScheduleSnapshot) bool) monitorScheduleSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state := monitorScheduleState(t, srv, "")
		if matches(state) {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("monitor schedule did not reach the expected state")
	return monitorScheduleSnapshot{}
}

func drainMonitorConfigurationWake(srv *Server) {
	select {
	case <-srv.monitorWake:
	default:
	}
}
