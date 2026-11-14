package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func assertTOTPAuthorizationExportsBlocked(t *testing.T, s *Server, m *jobs.Manager, job *jobs.Job) {
	t.Helper()
	if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "historical authorization")
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "pages": 1, "created": 1}})
	}))
	t.Cleanup(remote.Close)
	requests := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/jobs/" + job.ID + "/download", nil},
		{http.MethodPost, "/api/jobs/download-batch", map[string]any{"ids": []string{job.ID}}},
		{http.MethodPost, "/api/jobs/export-source", map[string]any{"ids": []string{job.ID}}},
		{http.MethodPost, "/api/sub2api/upload", map[string]any{"ids": []string{job.ID}, "config": map[string]any{"baseUrl": remote.URL, "adminApiKey": "fixture-admin-key"}}},
	}
	for _, req := range requests {
		response := frontendRequest(s, req.method, req.path, req.body, "test")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "2FA") || strings.Contains(response.Body.String(), "synthetic-access") {
			t.Errorf("recovery exported authorization through %s: status=%d body=%s", req.path, response.Code, response.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("blocked authorization reached the remote upload service")
	}
	current, _ := m.Get(job.ID)
	public := s.publicJob(current)
	if public["canDownload"] != false || public["totpExportBlocked"] != true || public["totpExportUnavailableReason"] == "" {
		t.Fatal("public capabilities offered an account-level blocked export")
	}
}

func TestTOTPRecoveryBlocksHistoricalAuthorizationExportsWithoutChangingEvidence(t *testing.T) {
	s, m, job, vault := recoveryAPIWithVault(t)
	path := filepath.Join(s.cfg.OutputRoot, job.ID, "totp-setup-result.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := vault.Load(job.Email)
	if err != nil {
		t.Fatal(err)
	}
	assertTOTPAuthorizationExportsBlocked(t, s, m, job)
	after, readErr := os.ReadFile(path)
	afterCredential, loadErr := vault.Load(job.Email)
	if readErr != nil || loadErr != nil || string(before) != string(after) || !reflect.DeepEqual(credential, afterCredential) || m.QueuePosition(job.ID) != 0 {
		t.Fatal("export admission changed recovery evidence or queued work", readErr, loadErr)
	}
}

func TestTOTPQueuedPeerBlocksHistoricalAuthorizationExports(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(&monitorMemoryVault{values: map[string]credentials.Credentials{}})
	owner := importTOTPActionFixture(t, m, "export-owner", true)
	peer, err := m.Submit(jobs.Spec{ID: "export-peer", Email: owner.Email, ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(owner.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	assertTOTPAuthorizationExportsBlocked(t, s, m, peer)
}

func TestTOTPExportGatePreservesOrdinaryHistoricalAuthorization(t *testing.T) {
	s, m := newMonitorTestServer(t)
	job := importTOTPActionFixture(t, m, "ordinary-export", false)
	if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "historical authorization")
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/jobs/" + job.ID + "/download", nil},
		{http.MethodPost, "/api/jobs/download-batch", map[string]any{"ids": []string{job.ID}}},
	} {
		response := frontendRequest(s, req.method, req.path, req.body, "test")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "synthetic-access") {
			t.Fatal("ordinary historical export was blocked", response.Code, response.Body.String())
		}
	}
	current, _ := m.Get(job.ID)
	if s.publicJob(current)["canDownload"] != true {
		t.Fatal("ordinary historical export is unavailable in UI")
	}
}

func TestTOTPManualPayloadUploadChecksKnownAccountRecovery(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending-known-account", false: "unrelated-external-account"}[known], func(t *testing.T) {
			s, _, job, _ := recoveryAPIWithVault(t)
			var calls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "pages": 1, "created": 1}})
			}))
			defer remote.Close()
			email := job.Email
			if !known {
				email = "external@example.test"
			}
			payload := map[string]any{"accounts": []any{map[string]any{"name": email, "platform": "openai", "type": "oauth", "credentials": map[string]any{"email": email, "access_token": "synthetic-access", "refresh_token": "synthetic-refresh"}}}}
			response := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{"payload": payload, "baseUrl": remote.URL, "adminApiKey": "fixture-admin-key"}, "test")
			if !known {
				if response.Code != http.StatusOK || calls.Load() == 0 {
					t.Fatal("external account upload required a local account", response.Code, response.Body.String())
				}
				return
			}
			var result struct {
				Outcome   string
				Uploaded  int
				Uncertain bool
			}
			if json.Unmarshal(response.Body.Bytes(), &result) != nil || response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "2FA") || result.Outcome != "blocked" || result.Uploaded != 0 || result.Uncertain || calls.Load() != 0 {
				t.Fatal("manual payload bypassed account recovery or misreported a write", response.Code, response.Body.String(), calls.Load())
			}
		})
	}
}

func TestTOTPUnfinishedJournalBlocksHistoricalAuthorizationExports(t *testing.T) {
	s, m, job := preparedTOTPAPIFixture(t)
	current, _ := m.Get(job.ID)
	if !current.TOTPRecoveryPending || m.TOTPResetCompletionPending(job.ID) {
		t.Fatal("unfinished evidence must remain isolated without a completion capability")
	}
	assertTOTPAuthorizationExportsBlocked(t, s, m, job)
}

func TestTOTPDownloadRejectsHistoricalGrantAfterCompletedLogout(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	job := importTOTPActionFixture(t, m, "completed-logout-download", true)
	if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "historical authorization")
	m.SetRunner(func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
		if err := m.StartTOTPWorkflowStep(ctx, active.ID, "totp"); err != nil {
			return err
		}
		if err := beginConsoleTOTPReset(t, s.cfg.OutputRoot, m, ctx, active.ID, "completed-logout-operation"); err != nil {
			return err
		}
		credential, err := vault.Load(active.Email)
		if err != nil {
			return err
		}
		credential.TOTPSecret, credential.TOTPResetPending = "GEZDGNBVGY3TQOJQ", false
		if err := vault.Save(active.Email, credential); err != nil {
			return err
		}
		writeCommittedResetFixture(t, s.cfg.OutputRoot, m, vault, active.ID, credential.TOTPResetOperationID)
		if err := m.CommitTOTPReset(ctx, active.ID, credential); err != nil {
			return err
		}
		if err := m.MarkTOTPResetCommitted(ctx, active.ID, credential.TOTPResetOperationID); err != nil {
			return err
		}
		if err := m.StartTOTPWorkflowStep(ctx, active.ID, "logout"); err != nil {
			return err
		}
		if err := m.UpdateTOTPResetResult(active.ID, map[string]any{"logout_all_devices_requested": true, "logout_all_devices_status": "succeeded"}, nil); err != nil {
			return err
		}
		if err := finishConsoleTOTPReset(t, s.cfg.OutputRoot, m, active.ID, credential.TOTPResetOperationID); err != nil {
			return err
		}
		return m.FinishTOTPWorkflow(active.ID, nil)
	})
	if _, err := m.QueueOperation(job.ID, "reset-2fa", map[string]any{"logoutAllDevicesAfterTotp": true}); err != nil {
		t.Fatal(err)
	}
	m.Start()
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	waitConsoleState(t, m, job.ID, func(j *jobs.Job) bool { return j.FinishedAt != nil && !m.ActiveAccountOperation(job.ID) })
	current, _ := m.Get(job.ID)
	if current.OperationStatus != "succeeded" || current.TOTPWorkflowStatus != "succeeded" || current.LogoutAllDevicesStatus != "succeeded" || current.ResultSaved || current.TOTPRecoveryPending || m.CheckTOTPExport(job.ID) != nil {
		t.Fatal("fixture did not isolate an invalidated grant after completed logout")
	}
	response := frontendRequest(s, http.MethodGet, "/api/jobs/"+job.ID+"/download", nil, "test")
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "synthetic-access") || response.Header().Get("Content-Disposition") != "" {
		t.Fatal("completed logout allowed stale authorization download", response.Code, response.Body.String())
	}
}

type invalidatingDownloadResponse struct {
	*httptest.ResponseRecorder
	once       sync.Once
	invalidate func()
}

func (w *invalidatingDownloadResponse) Header() http.Header {
	w.once.Do(w.invalidate)
	return w.ResponseRecorder.Header()
}

func TestTOTPDownloadRechecksGrantValidityAfterAdmission(t *testing.T) {
	s, m := newMonitorTestServer(t)
	job := importTOTPActionFixture(t, m, "invalidated-during-download", false)
	if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "historical authorization")
	// Header preparation starts only after initial admission. Invalidate the
	// grant there so the check after reading must reject the captured bytes.
	w := &invalidatingDownloadResponse{ResponseRecorder: httptest.NewRecorder(), invalidate: func() { m.UpdateResult(job.ID, false, "authorization invalidated") }}
	s.downloadJob(w, httptest.NewRequest(http.MethodGet, "/api/jobs/"+job.ID+"/download", nil), job.ID)
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "synthetic-access") || w.ResponseRecorder.Header().Get("Content-Disposition") != "" {
		t.Fatal("download returned authorization invalidated after initial admission", w.Code, w.Body.String())
	}
}
