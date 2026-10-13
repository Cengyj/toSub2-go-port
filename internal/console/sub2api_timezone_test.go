package console

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/jobs"
)

func TestSub2APIUploadRequestTimezoneFromConsoleSettings(t *testing.T) {
	for _, source := range []string{"ids", "payload"} {
		for _, timezone := range []string{"", "Asia/Shanghai"} {
			t.Run(source+"/"+timezone, func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				want := timezone
				if want == "" {
					want = "America/Los_Angeles"
				}
				var uploads atomic.Int32
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts" {
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "pages": 1}})
						return
					}
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/accounts/batch" {
						t.Error("unexpected upload route", r.Method, r.URL.Path)
					}
					var body struct {
						Accounts []map[string]any `json:"accounts"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Accounts) != 1 {
						t.Error("unexpected upload payload", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					extra, _ := body.Accounts[0]["extra"].(map[string]any)
					if extra["openai_request_timezone"] != want {
						t.Error("upload omitted or ignored request timezone", extra["openai_request_timezone"])
					}
					if source == "payload" && extra["retain"] != true {
						t.Error("upload replaced unrelated extra settings")
					}
					uploads.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"created": 1}})
				}))
				defer remote.Close()
				config := map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-key"}
				if timezone != "" {
					config["openaiRequestTimezone"] = timezone
				}
				body := map[string]any{"config": config}
				if source == "ids" {
					job, err := m.Submit(jobs.Spec{ID: "timezone-upload", Email: "timezone@example.test", ImportOnly: true})
					if err != nil {
						t.Fatal(err)
					}
					m.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
					m.UpdateResult(job.ID, true, "")
					if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
						t.Fatal(err)
					}
					body["ids"] = []string{job.ID}
				} else {
					body["payload"] = map[string]any{"accounts": []any{map[string]any{
						"platform": "openai", "type": "oauth", "credentials": map[string]any{"email": "timezone@example.test", "access_token": "fixture-token"},
						"extra": map[string]any{"retain": true, "openai_request_timezone": "Europe/London"},
					}}}
				}
				response := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", body, "test")
				if response.Code != http.StatusOK || uploads.Load() != 1 {
					t.Fatal("timezone upload failed", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestMonitorRequestTimezonePersistsRestoresAndPreservesPartialUpdates(t *testing.T) {
	s, m := newMonitorTestServer(t)
	post := func(body any) string {
		t.Helper()
		response := frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", body, "test")
		if response.Code != http.StatusOK {
			t.Fatal("monitor timezone save failed", response.Code, response.Body.String())
		}
		var result struct {
			Timezone string `json:"openaiRequestTimezone"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Timezone
	}
	if s.publicMonitorState()["openaiRequestTimezone"] != "America/Los_Angeles" {
		t.Fatal("new monitor has no default request timezone")
	}
	if got := post(map[string]any{"enabled": false, "config": map[string]any{"openaiRequestTimezone": " Asia/Shanghai "}}); got != "Asia/Shanghai" {
		t.Fatal("custom timezone not saved", got)
	}
	if got := post(map[string]any{"enabled": false, "config": map[string]any{"groupIds": []int{7}}}); got != "Asia/Shanghai" {
		t.Fatal("partial settings reset timezone", got)
	}
	restored := NewServer(s.cfg, m, embed.FS{}, s.logger)
	t.Cleanup(restored.Close)
	if restored.monitorConfig.OpenAIRequestTimezone != "Asia/Shanghai" || restored.monitor.Configuration().OpenAIRequestTimezone != "Asia/Shanghai" || restored.publicMonitorState()["openaiRequestTimezone"] != "Asia/Shanghai" {
		t.Fatal("saved request timezone was not restored")
	}
	if got := post(map[string]any{"enabled": false, "openaiRequestTimezone": "Europe/London"}); got != "Europe/London" {
		t.Fatal("top-level timezone not accepted", got)
	}
	if got := post(map[string]any{"enabled": false, "config": map[string]any{"openaiRequestTimezone": " "}}); got != "America/Los_Angeles" {
		t.Fatal("explicit blank did not restore default", got)
	}
}

func TestSub2APIInvalidRequestTimezoneDoesNotSendHTTPOrChangeMonitor(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	response := frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", map[string]any{"enabled": false, "config": map[string]any{"openaiRequestTimezone": "Asia/Shanghai"}}, "test")
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer remote.Close()
	for _, path := range []string{"/api/sub2api/upload", "/api/sub2api/monitor"} {
		for _, invalid := range []any{"Invalid/Zone", "Local", nil, 1, true, []any{"Asia/Shanghai"}, map[string]any{}} {
			body := map[string]any{"enabled": false, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-key", "openaiRequestTimezone": invalid}, "payload": map[string]any{"accounts": []any{}}}
			response := frontendRequest(s, http.MethodPost, path, body, "test")
			if response.Code != http.StatusBadRequest || requests.Load() != 0 || s.monitorConfig.OpenAIRequestTimezone != "Asia/Shanghai" {
				t.Fatal("invalid timezone made a request or changed settings", path, response.Code)
			}
		}
	}
}
