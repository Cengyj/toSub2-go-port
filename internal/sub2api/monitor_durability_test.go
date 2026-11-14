package sub2api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/privatefile"
)

func TestMonitorConfigurePublishesOnlyMatchingPersistedPolicy(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "uncertain"}[uncertain], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "monitor.json")
			initial := MonitorConfig{BaseURL: "https://old.example", GroupIDs: []int{1}, Enabled: false, Cooldown: time.Minute, CooldownConfigured: true}
			service := NewMonitorService(New(Config{BaseURL: initial.BaseURL, APIKey: "old"}), initial, MonitorHooks{}, path)
			if err := service.Save(); err != nil {
				t.Fatal(err)
			}
			old := service.Configuration()
			service.State.Permanent["owner@example.test"] = "blocked"
			failure := errors.New("fixture write rejected")
			service.stateWriter = func(ctx context.Context, path string, data []byte) error {
				if uncertain {
					if err := privatefile.Write(ctx, path, data); err != nil {
						return err
					}
					return privatefile.ErrDurabilityUncertain
				}
				return failure
			}
			next := old
			next.BaseURL = "https://new.example"
			next.GroupIDs = []int{2}
			next.Enabled = true
			client := New(Config{BaseURL: next.BaseURL, APIKey: "new"})
			err := service.Configure(client, next, MonitorHooks{})
			if err == nil {
				t.Fatal("failed persistence reported saved")
			}
			if uncertain {
				if !errors.Is(err, privatefile.ErrDurabilityUncertain) || service.Configuration().BaseURL != next.BaseURL || service.Client != client {
					t.Fatal("visible replacement differs from active policy", err)
				}
			} else if !errors.Is(err, failure) || !reflect.DeepEqual(service.Configuration(), old) || service.Client == client || service.State.Permanent["owner@example.test"] != "blocked" {
				t.Fatal("rejected policy changed active configuration", err)
			}
			readback := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path).Configuration()
			if readback.BaseURL != service.Configuration().BaseURL || !reflect.DeepEqual(readback.GroupIDs, service.Configuration().GroupIDs) || readback.Enabled != service.Configuration().Enabled {
				t.Fatal("disk and active policy diverged")
			}
			data, err := os.ReadFile(path)
			if err != nil || len(data) == 0 {
				t.Fatal(err)
			}
		})
	}
}

func TestSyncAccountCredentialsReusesOnlyExactGrantAndPreservesDisabled(t *testing.T) {
	for _, changed := range []bool{false, true} {
		for _, status := range []string{"error", "disabled"} {
			t.Run(status+map[bool]string{false: "_same", true: "_changed"}[changed], func(t *testing.T) {
				var steps []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					steps = append(steps, r.Method+" "+r.URL.Path)
					if r.Method == http.MethodGet {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"data":{"id":12,"status":"` + status + `","credentials":{"email":"owner@example.test","access_token":"access","refresh_token":"old","upstream_specific":true},"extra":{"codex_fingerprint_mode":"default","openai_request_timezone":"Asia/Shanghai"}}}`))
						return
					}
					_, _ = w.Write([]byte(`{}`))
				}))
				defer server.Close()
				refresh := "old"
				if changed {
					refresh = "new"
				}
				err := New(Config{BaseURL: server.URL, APIKey: "fixture", OpenAIRequestTimezone: "Asia/Shanghai"}).SyncAccountCredentials(t.Context(), 12, map[string]any{"email": "owner@example.test", "access_token": "access", "refresh_token": refresh}, "default")
				if err != nil {
					t.Fatal(err)
				}
				expected := []string{"GET /api/v1/admin/accounts/12"}
				if changed {
					expected = append(expected, "PUT /api/v1/admin/accounts/12")
				}
				if status == "error" {
					expected = append(expected, "POST /api/v1/admin/accounts/12/clear-error", "POST /api/v1/admin/accounts/12/schedulable")
				}
				if !reflect.DeepEqual(steps, expected) {
					t.Fatalf("requests %v, want %v", steps, expected)
				}
			})
		}
	}
}
