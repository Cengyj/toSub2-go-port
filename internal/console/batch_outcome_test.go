package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestBatchSubmissionReportsUncertainCommitSeparately(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "unknown"
		if mixed {
			name = "partial"
		}
		t.Run(name, func(t *testing.T) {
			failures := []batchSubmissionFailure{{ID: "unknown", Error: batchSubmissionMessage(jobs.ErrAdmissionUncertain), Uncertain: true}}
			queued := []map[string]any{}
			selected, wantFailed, wantStatus := 1, 0, http.StatusConflict
			if mixed {
				queued = append(queued, map[string]any{"id": "queued"})
				failures = append(failures, batchSubmissionFailure{ID: "failed", Error: batchSubmissionMessage(jobs.ErrAdmissionPersistence)})
				selected, wantFailed, wantStatus = 3, 1, http.StatusOK
			}
			w := httptest.NewRecorder()
			writeBatchSubmissionResult(w, selected, 0, queued, failures)
			var result batchSubmissionResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != wantStatus || result.OK || !result.Uncertain || result.Outcome != name || result.Unknown != 1 || result.Failed != wantFailed || result.Selected != result.Queued+result.Failed+result.Unknown+result.Skipped {
				t.Fatal("uncertain commit was collapsed into definite failure or success")
			}
			if !strings.Contains(result.Failures[0].Error, "避免重复提交") || !strings.Contains(result.Failures[0].Error, "重启") {
				t.Fatal("uncertain result lost restart and retry guidance")
			}
		})
	}
}

func TestBatchSubmissionPersistenceFailureDoesNotClaimQueued(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	j, err := m.Submit(jobs.Spec{ID: "disk-failure", Email: "disk-failure@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(s.cfg.OutputRoot, "job-meta.json")
	if err := os.Rename(metadata, metadata+".before"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	w := frontendRequest(s, http.MethodPost, "/api/jobs/relogin-batch", map[string]any{"ids": []string{j.ID}}, "test")
	var result batchSubmissionResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || result.OK || result.Outcome != "failed" || result.Failed != 1 || result.Queued != 0 || result.Skipped != 0 {
		t.Fatal("failed disk commit was not reported as a submission failure", w.Code)
	}
	current, _ := m.Get(j.ID)
	if current.Status != jobs.StatusImported || m.QueuePosition(j.ID) != 0 {
		t.Fatal("failed disk commit left executable work")
	}
	if strings.Contains(w.Body.String(), s.cfg.OutputRoot) {
		t.Fatal("private storage path leaked through batch result")
	}
}

func TestBatchReauthorizationPrevalidatesEntireSelection(t *testing.T) {
	s, m := newMonitorTestServer(t)
	for _, id := range []string{"ready", "busy"} {
		if _, err := m.Submit(jobs.Spec{ID: id, Email: id + "@example.test", ImportOnly: id == "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	w := frontendRequest(s, http.MethodPost, "/api/jobs/reauthorize-batch", map[string]any{"ids": []string{"ready", "busy"}}, "test")
	if w.Code != http.StatusConflict || m.QueuePosition("ready") != 0 {
		t.Fatal("ineligible selection was partially queued before prevalidation")
	}
}

func TestBatchSubmissionFailureHidesInternalDetails(t *testing.T) {
	const canary = "synthetic-private-value-must-not-be-public"
	message := batchSubmissionMessage(errors.New("write /private/" + canary + " failed"))
	if message == "" || strings.Contains(message, canary) || strings.Contains(message, "/private/") {
		t.Fatal("backend error details were exposed to the batch client")
	}
}

func TestBatchImportFailureRetainsCompletedReceipt(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	w := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{
		"jobs": []map[string]any{
			{"id": "same-id", "email": "first@example.test"},
			{"id": "same-id", "email": "second@example.test"},
			{"id": "unprocessed", "email": "third@example.test"},
		},
	}, "test")
	var result struct {
		Outcome                                                             string `json:"outcome"`
		Error                                                               string `json:"error"`
		Created, Updated, Completed, Failed, Unknown, Unprocessed, Selected int
		Jobs                                                                []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || result.Outcome != "partial" || result.Created != 1 || result.Updated != 0 || result.Completed != 1 || result.Failed != 1 || result.Unknown != 0 || result.Unprocessed != 1 || result.Selected != 3 || len(result.Jobs) != 1 {
		t.Fatalf("partial import lost its completed receipt: status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(result.Error, "已保存 1 条") || !strings.Contains(result.Error, "尚未处理 1 条") {
		t.Fatalf("partial import has no actionable counts: %q", result.Error)
	}
	first, exists := m.Get("same-id")
	if !exists || first.Email != "first@example.test" || first.Status != jobs.StatusImported || m.QueuePosition(first.ID) != 0 {
		t.Fatal("successful import was lost or started authorization")
	}
	if _, exists := m.Get("unprocessed"); exists {
		t.Fatal("import continued after the reported failure")
	}
}

func TestBatchImportReceiptAccountsForUnknownAndUnprocessedRows(t *testing.T) {
	for _, completed := range []int{0, 2} {
		imported := make([]map[string]any, completed)
		for i := range imported {
			imported[i] = map[string]any{"id": "synthetic"}
		}
		w := httptest.NewRecorder()
		writeBatchImportResult(w, 4, completed, 0, imported, jobs.ErrAdmissionUncertain)
		var result batchImportResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		wantOutcome := "unknown"
		if completed > 0 {
			wantOutcome = "partial"
		}
		if w.Code != http.StatusConflict || result.OK || !result.Uncertain || result.Outcome != wantOutcome || result.Unknown != 1 || result.Failed != 0 || result.Completed != completed || result.StoppedAt != completed+1 || result.Selected != result.Completed+result.Failed+result.Unknown+result.Unprocessed {
			t.Fatal("uncertain import was collapsed into definite failure or completion", result)
		}
		if !strings.Contains(result.Error, "避免直接重试") {
			t.Fatal("unknown import omitted safe retry guidance")
		}
	}
}

func TestBatchImportReceiptPreservesSuccessFieldsAndHidesPrivateFailure(t *testing.T) {
	imported := []map[string]any{{"id": "new"}, {"id": "updated"}}
	w := httptest.NewRecorder()
	writeBatchImportResult(w, 2, 1, 1, imported, nil)
	var result batchImportResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusCreated || !result.OK || result.Outcome != "completed" || result.Created != 1 || result.Updated != 1 || result.Completed != 2 || result.StoppedAt != 0 || len(result.Jobs) != 2 || result.Error != "" {
		t.Fatal("successful import changed its existing response fields", result)
	}
	w = httptest.NewRecorder()
	writeBatchImportResult(w, 1, 0, 0, []map[string]any{}, errors.New("write /private/synthetic-secret-value failed"))
	if strings.Contains(w.Body.String(), "synthetic-secret") || strings.Contains(w.Body.String(), "/private/") {
		t.Fatal("import failure leaked internal details")
	}
}

func TestBatchSubmissionSeparatesQueuedFailedAndSkipped(t *testing.T) {
	for _, action := range []string{"relogin", "reauthorize", "setup-2fa", "add-password"} {
		scenarios := []string{"partial", "failed", "queued"}
		if action != "reauthorize" {
			scenarios = append(scenarios, "skipped")
		}
		for _, scenario := range scenarios {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				vault := credentials.NewMemoryStore()
				m.SetCredentialStore(vault)
				for _, id := range []string{"ready", "blocked", "busy"} {
					if _, err := m.Submit(jobs.Spec{ID: id, Email: id + "@example.test", ImportOnly: id != "busy"}); err != nil {
						t.Fatal(err)
					}
				}
				// Model a protected-store recovery marker newer than the public
				// account snapshot. The manager must reject this submission.
				if err := vault.Save("blocked@example.test", credentials.Credentials{TOTPResetPending: true}); err != nil {
					t.Fatal(err)
				}
				ids := []string{"ready", "blocked", "busy", "ready"}
				wantStatus, wantQueued, wantFailed, wantSkipped := http.StatusOK, 1, 1, 1
				if action == "reauthorize" {
					// Reauthorization intentionally prevalidates every selection.
					ids, wantSkipped = []string{"ready", "blocked", "ready"}, 0
				}
				switch scenario {
				case "failed":
					ids, wantStatus, wantQueued, wantFailed, wantSkipped = []string{"blocked"}, http.StatusConflict, 0, 1, 0
				case "skipped":
					ids, wantStatus, wantQueued, wantFailed, wantSkipped = []string{"busy"}, http.StatusConflict, 0, 0, 1
				case "queued":
					ids, wantQueued, wantFailed, wantSkipped = []string{"ready"}, 1, 0, 0
				}
				w := frontendRequest(s, http.MethodPost, "/api/jobs/"+action+"-batch", map[string]any{"ids": ids}, "test")
				var got struct {
					OK                                         bool   `json:"ok"`
					Outcome                                    string `json:"outcome"`
					Selected, Started, Queued, Failed, Skipped int
					Jobs                                       []struct {
						ID string `json:"id"`
					} `json:"jobs"`
					Failures []struct {
						ID    string `json:"id"`
						Error string `json:"error"`
					} `json:"failures"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if w.Code != wantStatus || got.Queued != wantQueued || got.Failed != wantFailed || got.Skipped != wantSkipped || got.Outcome != scenario {
					t.Fatalf("status=%d outcome=%q queued=%d failed=%d skipped=%d; want %d/%s/%d/%d/%d", w.Code, got.Outcome, got.Queued, got.Failed, got.Skipped, wantStatus, scenario, wantQueued, wantFailed, wantSkipped)
				}
				if got.Selected != wantQueued+wantFailed+wantSkipped || got.Started != got.Queued || len(got.Jobs) != got.Queued || len(got.Failures) != got.Failed || got.OK != (wantFailed == 0 && wantQueued > 0) {
					t.Fatal("batch outcome counts or success flag are inconsistent")
				}
				if wantFailed > 0 && (got.Failures[0].ID != "blocked" || got.Failures[0].Error == "") {
					t.Fatal("failed selection has no actionable result")
				}
				blocked, _ := m.Get("blocked")
				if blocked.Status != jobs.StatusImported || m.QueuePosition("blocked") != 0 {
					t.Fatal("failed submission changed the protected account")
				}
				if wantQueued > 0 && m.QueuePosition("ready") == 0 {
					t.Fatal("confirmed queued account was lost")
				}
			})
		}
	}
}
