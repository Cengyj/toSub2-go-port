package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManualUploadPreservesConfirmedAndUnknownOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, reply, outcome     string
		created, failed, unknown float64
		uncertain                bool
	}{
		{"unconfirmed", `{}`, "unknown", 0, 0, 2, true},
		{"refused", `{"success":false,"error":"response-private-canary"}`, "failed", 0, 2, 0, false},
		{"partial-unknown", `{"created":1}`, "partial", 1, 0, 1, true},
		{"partial-refused", `{"created":1,"failed":1}`, "partial", 1, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"data":{"items":[],"pages":1,"total":0}}`))
					return
				}
				writes.Add(1)
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer remote.Close()
			s, _ := newMonitorTestServer(t)
			accounts := []any{}
			for _, email := range []string{"first@example.test", "second@example.test"} {
				accounts = append(accounts, map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{"email": email, "access_token": "grant-private-canary"}})
			}
			response := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{
				"config":  map[string]any{"baseUrl": remote.URL, "adminApiKey": "admin-private-canary"},
				"payload": map[string]any{"accounts": accounts},
			}, "test")
			var got struct {
				OK, Uncertain               bool
				Outcome                     string
				Selected, Uploaded, Skipped int
				Result                      map[string]float64
			}
			// Result also carries booleans/IDs; decode aggregate fields explicitly.
			var body map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			result := body["result"]
			delete(body, "result")
			header, _ := json.Marshal(body)
			if err := json.Unmarshal(header, &got); err != nil {
				t.Fatal(err)
			}
			var counts struct{ Created, Failed, Unknown float64 }
			if err := json.Unmarshal(result, &counts); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusBadGateway || got.OK || got.Outcome != tc.outcome || got.Uncertain != tc.uncertain || got.Selected != 2 || got.Uploaded != 2 || got.Skipped != 0 || counts.Created != tc.created || counts.Failed != tc.failed || counts.Unknown != tc.unknown || writes.Load() != 1 {
				t.Fatalf("inconsistent upload outcome: HTTP %d header=%+v counts=%+v writes=%d", response.Code, got, counts, writes.Load())
			}
			if strings.Contains(response.Body.String(), "private-canary") {
				t.Fatal("response exposed private upload material")
			}
		})
	}
}

func TestManualUploadLifecycleFailureKeepsCredentialUpdateEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, failStage, outcome  string
		status                    int
		updated, failed, unknown  int
		clearCalls, scheduleCalls int32
		uncertain                 bool
	}{
		{name: "credentials-rejected", failStage: "credentials", outcome: "failed", status: http.StatusBadRequest, failed: 1},
		{name: "credentials-unknown", failStage: "credentials", outcome: "unknown", status: http.StatusServiceUnavailable, unknown: 1, uncertain: true},
		{name: "clear-error-rejected", failStage: "clear-error", outcome: "partial", status: http.StatusBadRequest, updated: 1, failed: 1, clearCalls: 1},
		{name: "clear-error-unknown", failStage: "clear-error", outcome: "partial", status: http.StatusServiceUnavailable, updated: 1, unknown: 1, clearCalls: 1, uncertain: true},
		{name: "schedulable-rejected", failStage: "schedulable", outcome: "partial", status: http.StatusBadRequest, updated: 1, failed: 1, clearCalls: 1, scheduleCalls: 1},
		{name: "schedulable-unknown", failStage: "schedulable", outcome: "partial", status: http.StatusServiceUnavailable, updated: 1, unknown: 1, clearCalls: 1, scheduleCalls: 1, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var credentialCalls, clearCalls, scheduleCalls atomic.Int32
			account := map[string]any{
				"id": 12, "platform": "openai", "type": "oauth", "status": "error", "schedulable": false,
				"error_message": "401 credential-private-canary",
				"credentials":   map[string]any{"email": "owner@example.test", "access_token": "old-private-canary"},
			}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				stage := ""
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{account}, "pages": 1, "total": 1}})
					return
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": account})
					return
				case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
					credentialCalls.Add(1)
					stage = "credentials"
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/12/clear-error":
					clearCalls.Add(1)
					stage = "clear-error"
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/12/schedulable":
					scheduleCalls.Add(1)
					stage = "schedulable"
				default:
					t.Errorf("unexpected mutation or duplicate creation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if stage == tc.failStage {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"error":"response-private-canary"}`))
					return
				}
				_, _ = w.Write([]byte(`{"success":true}`))
			}))
			defer remote.Close()
			s, _ := newMonitorTestServer(t)
			response := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{
				"config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "admin-private-canary"},
				"payload": map[string]any{"accounts": []any{map[string]any{
					"platform": "openai", "type": "oauth",
					"credentials": map[string]any{"email": "owner@example.test", "access_token": "new-private-canary"},
				}}},
			}, "test")
			var got struct {
				OK, Uncertain               bool
				Outcome                     string
				Selected, Uploaded, Skipped int
				Result                      struct {
					Created, Updated, Success, Failed, Unknown int
					AccountIDs                                 []int `json:"account_ids"`
				}
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusBadGateway || got.OK || got.Outcome != tc.outcome || got.Uncertain != tc.uncertain || got.Selected != 1 || got.Uploaded != 1 || got.Skipped != 0 {
				t.Fatalf("inconsistent lifecycle upload outcome: HTTP %d body=%+v", response.Code, got)
			}
			if got.Result.Created != 0 || got.Result.Updated != tc.updated || got.Result.Success != 0 || got.Result.Failed != tc.failed || got.Result.Unknown != tc.unknown {
				t.Fatalf("lost or overstated confirmed credential progress: %+v", got.Result)
			}
			if len(got.Result.AccountIDs) != tc.updated || (tc.updated > 0 && got.Result.AccountIDs[0] != 12) {
				t.Fatalf("lost updated remote account identity: %v", got.Result.AccountIDs)
			}
			if credentialCalls.Load() != 1 || clearCalls.Load() != tc.clearCalls || scheduleCalls.Load() != tc.scheduleCalls {
				t.Fatalf("mutation replayed or crossed failure boundary: credentials=%d clear=%d schedule=%d", credentialCalls.Load(), clearCalls.Load(), scheduleCalls.Load())
			}
			if strings.Contains(response.Body.String(), "private-canary") || strings.Contains(response.Body.String(), "owner@example.test") {
				t.Fatal("response exposed private upload material")
			}
		})
	}
}
