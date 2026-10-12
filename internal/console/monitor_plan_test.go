package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

func TestMonitorPlanSettingsRoundTripPreserveAndClear(t *testing.T) {
	srv, _ := newMonitorTestServer(t)
	post := func(body string) []string {
		t.Helper()
		response := frontendRequest(srv, http.MethodPost, "/api/sub2api/monitor", json.RawMessage(body), "test")
		if response.Code != http.StatusOK {
			t.Fatalf("config failed: %d %s", response.Code, response.Body.String())
		}
		var result struct {
			ExcludedPlanTypes []string `json:"excludedPlanTypes"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.ExcludedPlanTypes
	}
	want := []string{"plus", "team", "unknown"}
	if got := post(`{"enabled":false,"config":{"excludedPlanTypes":[" PLUS ","business","team","unknown"]}}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("save=%v", got)
	}
	if got := post(`{"enabled":false,"config":{"groupIds":[7]}}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("omitted field erased selection: %v", got)
	}
	restored := sub2api.NewMonitorService(nil, sub2api.MonitorConfig{}, sub2api.MonitorHooks{}, srv.monitorFile())
	if !reflect.DeepEqual(restored.Configuration().ExcludedPlanTypes, want) {
		t.Fatal("exclusions not restored from saved settings")
	}
	if got := post(`{"enabled":false,"config":{"excludedPlanTypes":[]}}`); got == nil || len(got) != 0 {
		t.Fatalf("explicit clear=%v", got)
	}
	if got := post(`{"enabled":false,"excludedPlanTypes":["pro"]}`); !reflect.DeepEqual(got, []string{"pro"}) {
		t.Fatalf("top-level config=%v", got)
	}
	response := frontendRequest(srv, http.MethodPost, "/api/sub2api/monitor", json.RawMessage(`{"config":{"excludedPlanTypes":"plus"}}`), "test")
	if response.Code != http.StatusBadRequest || !reflect.DeepEqual(srv.monitor.Configuration().ExcludedPlanTypes, []string{"pro"}) {
		t.Fatal("invalid exclusion input changed existing settings")
	}
}

func TestMonitorPlanHookUsesLocalFallbackAndRemotePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, local, remote string
		excluded            []string
		want                bool
	}{
		{"local-fallback", "plus", "unknown", []string{"plus"}, true},
		{"remote-priority", "plus", "free", []string{"plus"}, false},
		{"remote-excluded", "free", "pro", []string{"pro"}, true},
		{"unknown-default", "unknown", "unknown", []string{"free"}, false},
		{"unknown-selected", "unknown", "unknown", []string{"unknown"}, true},
		{"business-alias", "business", "unknown", []string{"team"}, true},
		{"business-internal-plan", "free", "self_serve_business_prolite", []string{"business_premium"}, true},
		{"business-premium-distinct-from-team", "free", "self_serve_business_prolite", []string{"team"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			job, _ := manager.Submit(jobs.Spec{ID: "plan", Email: "plan@example.test", Input: map[string]any{"password": "fixture-password"}})
			manager.Cancel(job.ID)
			manager.RecordAuthorization(job.ID, true, "", map[string]bool{"password": true})
			writeMonitorOutputFixture(t, srv.cfg.OutputRoot, job.ID, []map[string]any{{"credentials": map[string]any{"email": job.Email, "plan_type": tc.local}}})
			manager.UpdateResult(job.ID, true, "")
			hooks := srv.monitorHooks(sub2api.Config{BaseURL: "https://example.test", ExcludedPlanTypes: tc.excluded})
			accounts := []sub2api.RemoteAccount{{ID: 8, Email: job.Email, PlanType: tc.remote}}
			decision, err := hooks.Evaluate(context.Background(), job.Email, accounts)
			if err != nil || (decision.Action == sub2api.ActionExcluded) != tc.want {
				t.Fatalf("decision=%+v err=%v", decision, err)
			}
			if tc.want {
				if err := hooks.StartRepair(context.Background(), job.Email, accounts); !errors.Is(err, sub2api.ErrPlanExcluded) {
					t.Fatalf("direct queue hook bypassed exclusion: %v", err)
				}
				current, _ := manager.Get(job.ID)
				if current.Status == jobs.StatusQueued || len(current.AutoRepairPendingAccountIDs) != 0 {
					t.Fatal("excluded account changed repair state")
				}
			}
		})
	}
}

func TestMonitorPlanFallbackExcludesWithoutBlockingManualRelogin(t *testing.T) {
	srv, manager := newMonitorTestServer(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":1,"email":"plan@example.test","status":"error"}]}}`))
	}))
	defer remote.Close()
	job, _ := manager.Submit(jobs.Spec{ID: "plan", Email: "plan@example.test", Input: map[string]any{"password": "fixture-password"}})
	manager.Cancel(job.ID)
	manager.RecordAuthorization(job.ID, true, "", map[string]bool{"password": true})
	writeMonitorOutputFixture(t, srv.cfg.OutputRoot, job.ID, []map[string]any{{"credentials": map[string]any{"email": job.Email, "plan_type": "plus"}}})
	manager.UpdateResult(job.ID, true, "")
	if err := srv.configureMonitor(sub2api.Config{BaseURL: remote.URL, APIKey: "fixture-admin", ExcludedPlanTypes: []string{"plus"}}, true, false); err != nil {
		t.Fatal(err)
	}
	result, err := srv.monitorCheck(context.Background(), "timer")
	if err != nil || result.ExcludedPlan != 1 || result.Started != 0 || result.Error != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/jobs/"+job.ID+"/relogin", strings.NewReader(`{}`))
	request.Host = "127.0.0.1"
	request.Header.Set("X-Console-Token", "test")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("excluded plan blocked manual relogin: %d %s", response.Code, response.Body.String())
	}
	current, _ := manager.Get(job.ID)
	if current.Status != jobs.StatusQueued || current.Operation != "relogin" || len(current.AutoRepairPendingAccountIDs) != 0 {
		t.Fatal("manual relogin changed into automatic repair")
	}
}
