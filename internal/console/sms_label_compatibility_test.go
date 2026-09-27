package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestSMSServiceLabelAcquisitionPersistenceAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, provider, label string
		config                map[string]any
		complete              bool
	}{
		{"luban", "luban", "121949", map[string]any{"serviceId": "121949"}, false},
		{"smsbower-country", "smsbower", "Fixture Country / $0.20", map[string]any{"service": "dr", "country": "1001", "countryLabel": "Fixture Country / $0.20"}, true},
		{"smsbower-fallback", "smsbower", "dr / 1001", map[string]any{"country": "1001"}, false},
		{"custom", "custom", "2 个号码", map[string]any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.provider == "luban" {
					if r.URL.Path == "/getNumber" {
						fmt.Fprint(w, "{\"code\":0,\"number\":\"+12025550123\",\"request_id\":\"fixture-order\"}")
					} else {
						fmt.Fprint(w, "{\"code\":0}")
					}
					return
				}
				if r.URL.Query().Get("action") == "getNumber" {
					fmt.Fprint(w, "ACCESS_NUMBER:fixture-order:12025550123")
				} else {
					fmt.Fprint(w, "ACCESS_READY")
				}
			}))
			t.Cleanup(remote.Close)
			s, m := newMonitorTestServer(t)
			j, err := m.Submit(jobs.Spec{ID: "sms-label", Email: "sms-label@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			m.Emit(jobs.JobEvent{JobID: j.ID, Stage: "phone", Status: jobs.StatusWaitingInput})
			tc.config["apiKey"], tc.config["baseUrl"] = "fixture-label-api-key", remote.URL+"/"
			if tc.provider == "custom" {
				tc.config["entries"] = []string{"+12025550123----" + remote.URL, "+12025550124----" + remote.URL}
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/sms-number", map[string]any{"providerId": tc.provider, "config": tc.config}, "test")
			var body struct {
				Job map[string]any
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || body.Job["smsServiceLabel"] != tc.label {
				t.Fatalf("service label was lost after acquisition: status=%d label=%v want=%q", w.Code, body.Job["smsServiceLabel"], tc.label)
			}
			b, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			var metadata struct{ Jobs []map[string]any }
			if json.Unmarshal(b, &metadata) != nil || len(metadata.Jobs) != 1 || metadata.Jobs[0]["sms_service_label"] != tc.label {
				t.Fatal("service label did not persist in compatible metadata")
			}
			s.smsMu.Lock()
			lease := s.smsLeases[j.ID]
			s.smsMu.Unlock()
			if lease == nil {
				t.Fatal("missing acquired fixture lease")
			}
			s.finishSMS(j.ID, lease, tc.complete, "")
			current, _ := m.Get(j.ID)
			label := s.publicJob(current)["smsServiceLabel"]
			if tc.complete && label != tc.label {
				t.Fatalf("completed order lost service label: %v", label)
			}
			if !tc.complete && label != "" {
				t.Fatalf("released order retained service label: %v", label)
			}
		})
	}
}

func TestSMSServiceLabelRestoresBothLegacyMetadataNames(t *testing.T) {
	for _, field := range []string{"sms_service_label", "luban_service_id"} {
		t.Run(field, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			metadata := map[string]any{"id": "legacy-sms-label", "email": "restore-label@example.test", "status": "failed", "sms_provider_id": "luban", "sms_provider_name": "LubanSMS", "sms_status": "error", field: "121949"}
			if err := st.WriteJSON(context.Background(), "job-meta.json", map[string]any{"jobs": []any{metadata}}); err != nil {
				t.Fatal(err)
			}
			s, m, _ := newPersistedSMSRestoreServer(t, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			j, ok := m.Get("legacy-sms-label")
			if !ok || s.publicJob(j)["smsServiceLabel"] != "121949" {
				t.Fatalf("legacy service label %s was not restored", field)
			}
		})
	}
}
