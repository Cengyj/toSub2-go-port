package console

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

func TestPublicJobSeparatesActionFailureFromAuthorizationAndCredentialWarning(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	started := time.Now().UTC()
	public := s.publicJob(&jobs.Job{
		ID: "fixture", Email: "owner@example.invalid", Status: jobs.StatusSucceeded,
		Operation: "add-password", Stage: "password_add", StartedAt: &started,
		ResultSaved: true, OperationStatus: "failed", OperationError: "fixture password action failed",
		LastError: "fixture credential store warning",
	})
	if public["operationStatus"] != "failed" || public["operationError"] != "fixture password action failed" || public["credentialWarning"] != "fixture credential store warning" || public["canDownload"] != true {
		t.Fatal("public account state conflates latest action failure with authorization availability or storage warning")
	}
	if public["stage"] != "password_add" || public["startedAt"] == nil {
		t.Fatal("public action state omitted stage or start time")
	}
}

func TestLogsEndpointIncludesStructuredOperationHistory(t *testing.T) {
	s, manager := newMonitorTestServer(t)
	j, err := manager.Submit(jobs.Spec{ID: "structured-logs", Email: "owner@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	manager.Emit(jobs.JobEvent{JobID: j.ID, Stage: "email_otp", Status: jobs.StatusWaitingInput, Prompt: "请输入邮箱验证码"})
	w := frontendRequest(s, http.MethodGet, "/api/jobs/"+j.ID+"/logs", nil, "test")
	var response struct {
		ID      string          `json:"id"`
		Logs    string          `json:"logs"`
		Entries []jobs.LogEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || response.ID != j.ID || response.Logs == "" || len(response.Entries) == 0 {
		t.Fatal("structured logs response omitted text, identity or entries")
	}
	last := response.Entries[len(response.Entries)-1]
	if last.At.IsZero() || last.OperationAt.IsZero() || last.Operation == "" || last.OperationStatus != "waiting" || last.Message != "请输入邮箱验证码" {
		t.Fatal("log entry cannot identify when, which action and which state produced its message")
	}
}

func TestAccountStatsCountUsableAuthorizationIndependentlyOfLatestAction(t *testing.T) {
	stats := jobStats([]*jobs.Job{
		{Status: jobs.StatusSucceeded, OperationStatus: "failed", ResultSaved: true},
		{Status: jobs.StatusCanceled, OperationStatus: "canceled", ResultSaved: true},
		{Status: jobs.StatusSucceeded, ResultSaved: true, TOTPRecoveryPending: true},
		{Status: jobs.StatusSucceeded},
	})
	if stats["available"] != 2 {
		t.Fatal("authorization summary counted unsafe output or discarded output after a later canceled action")
	}
}
