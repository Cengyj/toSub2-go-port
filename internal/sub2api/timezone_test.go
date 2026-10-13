package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestNormalizeOpenAIRequestTimezone(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", DefaultOpenAIRequestTimezone},
		{" \t\n ", DefaultOpenAIRequestTimezone},
		{" Asia/Shanghai ", "Asia/Shanghai"},
		{"America/Los_Angeles", "America/Los_Angeles"},
		{"UTC", "UTC"},
		{"Etc/GMT+8", "Etc/GMT+8"},
	} {
		got, err := NormalizeOpenAIRequestTimezone(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("timezone %q = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"Local", "local", "/etc/localtime", "../Asia/Shanghai", "Asia/../Shanghai", "Asia//Shanghai", "Asia\\Shanghai", "Asia/Shanghai\x00", "America/Unknown", "<script>"} {
		if _, err := NormalizeOpenAIRequestTimezone(raw); !errors.Is(err, ErrInvalidOpenAIRequestTimezone) {
			t.Errorf("invalid timezone %q accepted: %v", raw, err)
		}
	}
	if SafeError(ErrInvalidOpenAIRequestTimezone) != ErrInvalidOpenAIRequestTimezone.Error() {
		t.Fatal("timezone validation error lost its safe explanation")
	}
}

func TestBuildUploadTimezoneDefaultsAndPreservesSource(t *testing.T) {
	for _, tc := range []struct{ option, want string }{
		{"", DefaultOpenAIRequestTimezone},
		{" Asia/Shanghai ", "Asia/Shanghai"},
	} {
		account := uploadFixtureAccount("owner@example.test", "synthetic-access-token")
		account["extra"].(map[string]any)["openai_request_timezone"] = "UTC"
		payload, err := BuildUploadPayload([]map[string]any{account}, UploadOptions{OpenAIRequestTimezone: tc.option})
		if err != nil {
			t.Fatal(err)
		}
		extra := payload["accounts"].([]any)[0].(map[string]any)["extra"].(map[string]any)
		if extra["openai_request_timezone"] != tc.want || extra["plan_type"] != "pro" || account["extra"].(map[string]any)["openai_request_timezone"] != "UTC" {
			t.Fatal("upload timezone did not honor settings or mutated source metadata")
		}
	}
	if _, err := BuildUploadPayload([]map[string]any{uploadFixtureAccount("owner@example.test", "synthetic-access-token")}, UploadOptions{OpenAIRequestTimezone: "Local"}); !errors.Is(err, ErrInvalidOpenAIRequestTimezone) {
		t.Fatal("invalid upload option timezone accepted")
	}
}

func TestUploadTimezoneOnEveryNewAccount(t *testing.T) {
	for _, tc := range []struct{ name, config, incoming, want string }{
		{"default", "", "", DefaultOpenAIRequestTimezone},
		{"client-config", " Asia/Shanghai ", "", "Asia/Shanghai"},
		{"payload-option", "", "Europe/Paris", "Europe/Paris"},
		{"configured-overrides-old-payload", "Asia/Shanghai", "UTC", "Asia/Shanghai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts" {
					writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{}, "pages": 0, "total": 0}})
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/accounts/batch" {
					t.Error("unexpected upload request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var body struct{ Accounts []map[string]any }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body.Accounts) != 2 {
					t.Error("missing accounts")
				}
				for _, account := range body.Accounts {
					extra, _ := account["extra"].(map[string]any)
					if extra["openai_request_timezone"] != tc.want || extra["plan_type"] != "pro" {
						t.Error("new account request timezone or existing metadata missing")
					}
				}
				writeUploadFixture(t, w, map[string]any{"data": map[string]any{"created": 2, "failed": 0}})
			}))
			defer server.Close()
			first := uploadFixtureAccount("one@example.test", "synthetic-access-one")
			second := uploadFixtureAccount("two@example.test", "synthetic-access-two")
			if tc.incoming != "" {
				first["extra"].(map[string]any)["openai_request_timezone"] = tc.incoming
				second["extra"].(map[string]any)["openai_request_timezone"] = tc.incoming
			}
			if _, err := New(Config{BaseURL: server.URL, APIKey: "test", OpenAIRequestTimezone: tc.config}).Upload(context.Background(), uploadFixturePayload(first, second)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidTimezoneStopsBeforeRemoteRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	account := uploadFixtureAccount("owner@example.test", "synthetic-access-token")
	client := New(Config{BaseURL: server.URL, APIKey: "test", OpenAIRequestTimezone: "America/Invalid"})
	if _, err := client.Upload(context.Background(), uploadFixturePayload(account)); !errors.Is(err, ErrInvalidOpenAIRequestTimezone) {
		t.Fatalf("invalid client timezone accepted: %v", err)
	}
	if err := client.SyncAccountCredentials(context.Background(), 1, map[string]any{"access_token": "synthetic"}, ""); !errors.Is(err, ErrInvalidOpenAIRequestTimezone) {
		t.Fatalf("invalid repair timezone accepted: %v", err)
	}
	for _, invalid := range []any{"Local", "America/Invalid", nil, float64(1)} {
		account["extra"].(map[string]any)["openai_request_timezone"] = invalid
		if _, err := New(Config{BaseURL: server.URL, APIKey: "test", OpenAIRequestTimezone: "UTC"}).Upload(context.Background(), uploadFixturePayload(account)); !errors.Is(err, ErrInvalidOpenAIRequestTimezone) {
			t.Fatalf("invalid payload timezone accepted: %v", err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid timezone triggered remote requests")
	}
}

func TestMonitorTimezonePersistsRestoresAndRejectsInvalidUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub2api-monitor.json")
	monitor := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path)
	if monitor.Configuration().OpenAIRequestTimezone != DefaultOpenAIRequestTimezone || monitor.PublicState().OpenAIRequestTimezone != DefaultOpenAIRequestTimezone {
		t.Fatal("new monitor timezone default missing")
	}
	if err := monitor.Configure(nil, MonitorConfig{BaseURL: "http://localhost:18080", OpenAIRequestTimezone: " Asia/Shanghai "}, MonitorHooks{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted MonitorState
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.OpenAIRequestTimezone != "Asia/Shanghai" {
		t.Fatal("monitor timezone not saved")
	}
	restored := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path)
	if restored.Configuration().OpenAIRequestTimezone != "Asia/Shanghai" || restored.PublicState().OpenAIRequestTimezone != "Asia/Shanghai" {
		t.Fatal("monitor timezone not restored")
	}
	if err := restored.Configure(nil, MonitorConfig{OpenAIRequestTimezone: "Local"}, MonitorHooks{}); !errors.Is(err, ErrInvalidOpenAIRequestTimezone) {
		t.Fatal("monitor accepted an invalid timezone")
	}
	if restored.Configuration().OpenAIRequestTimezone != "Asia/Shanghai" {
		t.Fatal("invalid update changed previous monitor timezone")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"openaiRequestTimezone":"Local"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restored.Load(); !errors.Is(err, ErrInvalidOpenAIRequestTimezone) {
		t.Fatal("monitor accepted an invalid persisted timezone")
	}
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if restored = NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path); restored.Configuration().OpenAIRequestTimezone != DefaultOpenAIRequestTimezone {
		t.Fatal("old state without timezone did not receive default")
	}
}
