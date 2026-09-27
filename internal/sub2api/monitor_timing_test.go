package sub2api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestMonitorDurationDefaultsDistinguishExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		name               string
		cfg                MonitorConfig
		interval, cooldown time.Duration
	}{
		{"omitted", MonitorConfig{}, 5 * time.Minute, 5 * time.Minute},
		{"explicit-zero", MonitorConfig{CooldownConfigured: true}, 5 * time.Minute, 0},
		{"positive-legacy-config", MonitorConfig{Interval: time.Second, Cooldown: time.Hour}, time.Second, time.Hour},
		{"negative-fallback", MonitorConfig{Interval: -time.Second, Cooldown: -time.Second, CooldownConfigured: true}, 5 * time.Minute, 5 * time.Minute},
		{"interval-below-minimum", MonitorConfig{Interval: 30 * time.Millisecond}, 5 * time.Minute, 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor := NewMonitorService(nil, tc.cfg, MonitorHooks{}, "")
			assert := func() {
				t.Helper()
				got := monitor.Configuration()
				if got.Interval != tc.interval || got.Cooldown != tc.cooldown || !got.CooldownConfigured {
					t.Fatalf("normalized interval/cooldown=%v/%v", got.Interval, got.Cooldown)
				}
			}
			assert()
			if err := monitor.Configure(nil, tc.cfg, MonitorHooks{}); err != nil {
				t.Fatal(err)
			}
			assert()
			if err := monitor.Configure(nil, monitor.Configuration(), MonitorHooks{}); err != nil {
				t.Fatal(err)
			}
			assert()
		})
	}
}

func TestMonitorZeroCooldownIgnoresRestoredAndReconfiguredState(t *testing.T) {
	const email = "zero@example.test"
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{map[string]any{"id": 1, "email": email}}}})
	}))
	defer remote.Close()
	for _, restored := range []bool{false, true} {
		t.Run(map[bool]string{false: "configure", true: "restore"}[restored], func(t *testing.T) {
			client := New(Config{BaseURL: remote.URL, APIKey: "fixture"})
			cfg := MonitorConfig{BaseURL: remote.URL, Enabled: true, Cooldown: time.Hour}
			started := 0
			hooks := MonitorHooks{Evaluate: func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
				return RepairDecision{Action: ActionStart}, nil
			}, StartRepair: func(context.Context, string, []RemoteAccount) error { started++; return nil }}
			path := filepath.Join(t.TempDir(), "sub2api-monitor.json")
			monitor := NewMonitorService(client, cfg, hooks, path)
			monitor.markCooldown(email, "old persisted cooldown")
			monitor.markPermanent("permanent@example.test", "unavailable")
			if d := monitor.decision(context.Background(), email, nil); d.Action != ActionCooldown {
				t.Fatal("fixture cooldown was not established")
			}
			cfg.Cooldown = 0
			cfg.CooldownConfigured = true
			if restored {
				monitor = NewMonitorService(client, cfg, hooks, path)
			} else if err := monitor.Configure(client, cfg, hooks); err != nil {
				t.Fatal(err)
			}
			if len(monitor.State.Cooldowns) != 0 || monitor.State.Permanent["permanent@example.test"] == "" {
				t.Fatal("zero cooldown did not clear timers or discarded permanent state")
			}
			// A stale entry must also be harmless at decision time, even if a caller
			// populated it after configuration or loaded an old file out of band.
			monitor.State.Cooldowns[email] = CooldownEntry{Until: time.Now().Add(time.Hour), Reason: "stale"}
			for i := 0; i < 2; i++ {
				sum, err := monitor.Check(context.Background(), "manual")
				if err != nil || sum.Started != 1 || sum.Cooldown != 0 {
					t.Fatalf("disabled cooldown still enforced: %+v %v", sum, err)
				}
			}
			if started != 2 || len(monitor.State.Cooldowns) != 0 {
				t.Fatal("zero cooldown recreated a timer or suppressed another repair")
			}
		})
	}
}
