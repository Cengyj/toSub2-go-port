package console

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

func TestMaintenanceHTTPClosesAdmissionAndDrainsExistingPrompt(t *testing.T) {
	s, m := newMonitorTestServer(t)
	ready := make(chan struct{})
	release := make(chan struct{})
	m.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
		emit(jobs.JobEvent{JobID: j.ID, Status: "email_otp", Stage: "email_otp"})
		close(ready)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if _, err := m.Submit(jobs.Spec{ID: "maintained", Email: "maintained@synthetic.invalid"}); err != nil {
		t.Fatal(err)
	}
	m.Start()
	defer m.Shutdown(context.Background())
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("runner not started")
	}
	w := frontendRequest(s, http.MethodPost, "/api/maintenance", map[string]any{"enabled": true, "revision": 0}, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var state maintenanceStatus
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || !state.Enabled || state.Drained || state.Jobs.Running != 1 {
		t.Fatal("active task not reflected", w.Body.String())
	}
	w = frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "new@synthetic.invalid"}, "test")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("new batch admitted during maintenance", w.Code)
	}
	j, _ := m.Get("maintained")
	w = frontendRequest(s, http.MethodPost, "/api/jobs/maintained/input", map[string]any{"action": "email_otp", "value": "123456", "expectation": jobs.InputExpectation{OperationAt: j.LastOperationAt.Add(-time.Second), Stage: "email_otp"}}, "test")
	if w.Code != http.StatusConflict {
		t.Fatal("an old prompt generation was accepted during draining", w.Code)
	}
	w = frontendRequest(s, http.MethodPost, "/api/jobs/maintained/input", map[string]any{"action": "email_otp", "value": "123456", "expectation": jobs.InputExpectation{OperationAt: j.LastOperationAt, Stage: "email_otp"}}, "test")
	if w.Code != http.StatusOK {
		t.Fatal("existing prompt cannot drain", w.Code, w.Body.String())
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w = frontendRequest(s, http.MethodGet, "/api/maintenance", nil, "test")
		if json.Unmarshal(w.Body.Bytes(), &state) == nil && state.Drained {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !state.Drained {
		t.Fatal("completed task did not drain", w.Body.String())
	}
	w = frontendRequest(s, http.MethodPost, "/api/jobs/cancel-all", map[string]any{}, "test")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("late mutation crossed drained boundary", w.Code)
	}
	w = frontendRequest(s, http.MethodPost, "/api/maintenance", map[string]any{"enabled": false, "revision": 1}, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
}
