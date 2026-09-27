package console

import (
	"encoding/json"
	"net/http"
	"testing"

	"tosub2-go-port/internal/jobs"
)

func TestLegacySingleJobActionResponseContracts(t *testing.T) {
	for _, action := range []string{"cancel", "retry", "regenerate", "relogin", "setup-2fa", "add-password"} {
		t.Run(action, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			j, err := m.Submit(jobs.Spec{ID: "response", Email: "response@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			if action != "cancel" {
				m.Cancel(j.ID)
			}
			if action != "cancel" && action != "retry" {
				m.UpdateResult(j.ID, true, "")
				m.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusSucceeded})
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/"+action, map[string]any{}, "test")
			var body struct {
				Job *struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"job"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || body.Job == nil || body.Job.ID != j.ID {
				t.Fatalf("legacy action response changed: status=%d body=%s decode=%v", w.Code, w.Body.String(), err)
			}
			if action == "cancel" && body.Job.Status != jobs.StatusCanceled {
				t.Fatalf("cancel returned stale status %q", body.Job.Status)
			}
		})
	}
}

func TestLegacyLogsIncludeJobIdentity(t *testing.T) {
	s, m := newMonitorTestServer(t)
	j, err := m.Submit(jobs.Spec{ID: "logs", Email: "logs@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	w := frontendRequest(s, http.MethodGet, "/api/jobs/"+j.ID+"/logs", nil, "test")
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || body["id"] != j.ID {
		t.Fatalf("legacy logs identity missing: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := body["logs"].(string); !ok {
		t.Fatal("logs must remain a string")
	}
}

func TestLegacyJobActionsPreserveNoopCancelAndRetryErrors(t *testing.T) {
	s, m := newMonitorTestServer(t)
	j, err := m.Submit(jobs.Spec{ID: "terminal", Email: "terminal@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/retry", map[string]any{}, "test"); w.Code != http.StatusConflict {
		t.Fatalf("retrying an active task must return conflict: %d", w.Code)
	}
	if w := frontendRequest(s, http.MethodPost, "/api/jobs/missing/retry", map[string]any{}, "test"); w.Code != http.StatusNotFound {
		t.Fatalf("retrying a missing task must return not found: %d", w.Code)
	}
	m.Cancel(j.ID)
	m.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusSucceeded})
	w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/cancel", map[string]any{}, "test")
	var body struct {
		Job struct {
			Status string `json:"status"`
		} `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || body.Job.Status != jobs.StatusSucceeded {
		t.Fatalf("canceling an existing finished task must remain a no-op: %d %s", w.Code, w.Body.String())
	}
}

func TestLegacyReauthorizationRequestsRestart(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	for _, status := range []string{"reauth_required", "resume_available", jobs.StatusSucceeded, jobs.StatusFailed} {
		got := s.publicJob(&jobs.Job{ID: "reauth", Email: "reauth@example.test", Status: status})
		if got["restartRequired"] != (status == "reauth_required") {
			t.Errorf("status %s returned restartRequired=%v", status, got["restartRequired"])
		}
	}
}

func TestLegacyQueryCountsDistinctNormalizedEmails(t *testing.T) {
	s, m := newMonitorTestServer(t)
	if _, err := m.Submit(jobs.Spec{ID: "query", Email: "query@example.test"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path string
		body               any
		requested          int
	}{
		{"query-normalized", http.MethodPost, "/api/jobs/query", map[string]any{"emails": []string{"query@example.test", " QUERY@example.test ", "missing@example.test"}}, 2},
		{"unfiltered", http.MethodGet, "/api/jobs", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := frontendRequest(s, tc.method, tc.path, tc.body, "test")
			var body struct {
				Filter map[string]any `json:"filter"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || body.Filter["requested"] != float64(tc.requested) || body.Filter["matched"] != float64(1) {
				t.Fatalf("legacy filter counts changed: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
