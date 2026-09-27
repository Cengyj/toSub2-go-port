package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

func TestMonitorDurationEnvironmentMatchesLegacyNumbers(t *testing.T) {
	const key = "TOSUB2_MONITOR_DURATION_FIXTURE"
	fallback := 5 * time.Minute
	for _, tc := range []struct {
		name, value        string
		interval, cooldown time.Duration
	}{
		{"empty", "", fallback, fallback},
		{"whitespace", " \t\n", fallback, 0},
		{"zero", "0", fallback, 0},
		{"negative-zero", "-0", fallback, 0},
		{"negative", "-1", fallback, fallback},
		{"below-minimum", "999", fallback, 999 * time.Millisecond},
		{"fraction-below-minimum", "999.999", fallback, 999999 * time.Microsecond},
		{"minimum", "1000", time.Second, time.Second},
		{"fraction", " 1000.5 ", time.Second + 500*time.Microsecond, time.Second + 500*time.Microsecond},
		{"exponent", "1e3", time.Second, time.Second},
		{"hex", "0x3e8", time.Second, time.Second},
		{"binary", "0b1111101000", time.Second, time.Second},
		{"octal", "0o1750", time.Second, time.Second},
		{"leading-zero-decimal", "01000", time.Second, time.Second},
		{"bom-whitespace", "\ufeff1000\ufeff", time.Second, time.Second},
		{"nan", "NaN", fallback, fallback},
		{"infinite", "Infinity", fallback, fallback},
		{"go-inf", "+Inf", fallback, fallback},
		{"suffix", "1000ms", fallback, fallback},
		{"underscore", "1_000", fallback, fallback},
		{"hex-float", "0x1p10", fallback, fallback},
		{"signed-hex", "+0x3e8", fallback, fallback},
		{"missing-hex-digits", "0x", fallback, fallback},
		{"go-only-whitespace", "\u00851000\u0085", fallback, fallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.value)
			if got := monitorDuration(key, fallback, time.Second); got != tc.interval {
				t.Fatalf("interval=%v want=%v", got, tc.interval)
			}
			if got := monitorDuration(key, fallback, 0); got != tc.cooldown {
				t.Fatalf("cooldown=%v want=%v", got, tc.cooldown)
			}
		})
	}
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	if monitorDuration(key, fallback, 0) != fallback {
		t.Fatal("unset duration lost default")
	}
}

func TestMonitorZeroCooldownBypassesJobCooldownAndPendingRetry(t *testing.T) {
	t.Setenv("SUB2API_AUTO_REPAIR_COOLDOWN_MS", "0")
	s, manager := newMonitorTestServer(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": 7, "credentials": map[string]any{"email": "zero-cooldown@example.test"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer remote.Close()
	cfg := sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin-key"}
	if err := s.configureMonitor(cfg, true, false); err != nil {
		t.Fatal(err)
	}
	if current := s.monitor.Configuration(); current.Cooldown != 0 || !current.CooldownConfigured {
		t.Fatal("explicit zero became a default cooldown")
	}
	// The old public API rounds minute labels and always displays at least one;
	// this display value does not re-enable the internally disabled cooldown.
	if s.publicMonitorState()["cooldownMinutes"] != float64(1) {
		t.Fatal("legacy public cooldown label changed")
	}
	job, err := manager.Submit(jobs.Spec{Email: "zero-cooldown@example.test", Input: map[string]any{"password": "Synthetic-Password9!"}})
	if err != nil {
		t.Fatal(err)
	}
	manager.Cancel(job.ID)
	manager.RecordAuthorization(job.ID, true, "", map[string]bool{"password": true})
	manager.UpdateAutoRepair(job.ID, nil, sub2api.BackendIdentity(remote.URL), "previous attempt failed", false)
	hooks := s.monitorHooks(cfg)
	if d, err := hooks.Evaluate(context.Background(), job.Email, nil); err != nil || d.Action != sub2api.ActionStart {
		t.Fatalf("recent job timestamp retained disabled cooldown: %v %v", d, err)
	}
	manager.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
	manager.UpdateResult(job.ID, true, "")
	manager.UpdateAutoRepair(job.ID, []int{7}, sub2api.BackendIdentity(remote.URL), "previous transfer failed", false)
	if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
		t.Fatal(err)
	}
	if n, err := hooks.RetryPending(context.Background()); err != nil || n != 1 {
		t.Fatalf("disabled cooldown blocked pending transfer: %d %v", n, err)
	}
	current, _ := manager.Get(job.ID)
	if len(current.AutoRepairPendingAccountIDs) != 0 || current.AutoRepairLastSuccessAt == nil {
		t.Fatal("pending transfer was not completed")
	}
}

func TestMonitorPublicMinuteLabelsMatchLegacyRounding(t *testing.T) {
	for _, tc := range []struct {
		duration time.Duration
		want     float64
	}{{0, 1}, {time.Second, 1}, {89 * time.Second, 1}, {90 * time.Second, 2}, {5 * time.Minute, 5}} {
		if got := monitorPublicMinutes(tc.duration); got != tc.want {
			t.Fatalf("public minute label=%v want=%v", got, tc.want)
		}
	}
}
