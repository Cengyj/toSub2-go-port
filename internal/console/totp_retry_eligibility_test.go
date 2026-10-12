package console

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

const completedResetSeed = "GEZDGNBVGY3TQOJQ"

// These fixtures exercise durable manager transitions only. No scheduler or
// protocol runner starts, and all credentials belong to synthetic accounts.
func completeQueuedTOTPResetFixture(t *testing.T, m *jobs.Manager, vault credentials.Store, id, seed, logout string) {
	t.Helper()
	ctx := context.Background()
	if err := m.StartTOTPWorkflowStep(ctx, id, "totp"); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginTOTPReset(ctx, id); err != nil {
		t.Fatal(err)
	}
	j, _ := m.Get(id)
	c, err := vault.Load(j.Email)
	if err != nil {
		t.Fatal(err)
	}
	c.TOTPSecret, c.TOTPResetPending = seed, false
	if err := vault.Save(j.Email, c); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitTOTPReset(ctx, id, c); err != nil {
		t.Fatal(err)
	}
	if err := m.StartTOTPWorkflowStep(ctx, id, "logout"); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"logout_all_devices_status": logout}
	var operationErr error
	terminalStatus := jobs.StatusSucceeded
	if logout != "succeeded" {
		result["logout_all_devices_error"] = "HTTP 400 synthetic logout failure"
		operationErr = errors.New("synthetic logout was not confirmed")
		terminalStatus = jobs.StatusFailed
	}
	if err := m.UpdateTOTPResetResult(id, result, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishTOTPWorkflow(id, operationErr); err != nil {
		t.Fatal(err)
	}
	m.Emit(jobs.JobEvent{JobID: id, Stage: "complete", Status: terminalStatus})
}

func queueCompletedTOTPResetFixture(t *testing.T, m *jobs.Manager, vault credentials.Store, id, logout string) *jobs.Job {
	t.Helper()
	j := importTOTPActionFixture(t, m, id, true)
	m.UpdateResult(j.ID, true, "synthetic old grant")
	if _, err := m.QueueOperation(j.ID, "reset-2fa", map[string]any{
		"logoutAllDevicesAfterTotp": true,
		"reloginAfterTotp":          logout != "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	completeQueuedTOTPResetFixture(t, m, vault, j.ID, completedResetSeed, logout)
	return j
}

// Restoring terminal metadata mirrors the manager after a service restart and
// drops the fixture's unconsumed scheduler queue without starting any worker.
func restoreCompletedTOTPFixtureManager(t *testing.T, s *Server, vault credentials.Store) *jobs.Manager {
	t.Helper()
	st, err := store.New(s.cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	m := jobs.NewManager(context.Background(), 1, st, slog.Default())
	m.SetCredentialStore(vault)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.jobs = m
	return m
}

func assertCompletedTOTPResetRetryState(t *testing.T, s *Server, m *jobs.Manager, id, seed string) {
	t.Helper()
	j, ok := m.Get(id)
	if !ok || j.TOTPResetPhase != "credential_persisted" || !j.HasTOTPKey || j.TOTPRecoveryPending || j.TOTPCredentialInvalidated || j.ResultSaved {
		t.Fatal("fixture did not retain a saved new factor with an invalidated old grant")
	}
	if s.canSecurityOperation(j) {
		t.Fatal("fixture unexpectedly qualifies through the old grant/checkpoint eligibility path")
	}
	for _, name := range []string{"login-checkpoint.json", "sub2api-import-oauth.json", "totp-setup-result.json"} {
		if _, err := os.Stat(filepath.Join(s.cfg.OutputRoot, id, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected fixture artifact %s: %v", name, err)
		}
	}
	w := frontendRequest(s, http.MethodGet, "/api/jobs/"+id, nil, "test")
	var public map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil || w.Code != http.StatusOK {
		t.Fatalf("read reset eligibility: %d %s", w.Code, w.Body.String())
	}
	if public["canResetTotp"] != true || public["canSetupTotp"] != false || public["canDownload"] != false || public["totpResetUnavailableReason"] != "" {
		t.Fatal("completed saved reset incorrectly depends on the old authorization", public)
	}
	for _, secret := range []string{seed, resetAPISeed, "Reset-api-fixture9!"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("public reset eligibility leaked synthetic credentials")
		}
	}
}

func TestTOTPResetRetrySingleAfterSavedFactorAndLogoutOutcome(t *testing.T) {
	for _, logout := range []string{"failed", "unknown", "succeeded"} {
		t.Run(logout, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
			m.SetCredentialStore(vault)
			j := queueCompletedTOTPResetFixture(t, m, vault, "saved-reset", logout)
			m = restoreCompletedTOTPFixtureManager(t, s, vault)
			assertCompletedTOTPResetRetryState(t, s, m, j.ID, completedResetSeed)
			current, _ := m.Get(j.ID)
			if current.LogoutAllDevicesStatus != logout {
				t.Fatal("logout outcome was lost during restore")
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/setup-2fa", map[string]any{"resetTotp": true}, "test")
			queued, _ := m.Get(j.ID)
			if w.Code != http.StatusOK || queued.Status != jobs.StatusQueued || queued.Operation != "reset-2fa" || queued.TOTPWorkflowStatus != "pending" || queued.TOTPResetPhase != "initial" || m.QueuePosition(j.ID) != 1 {
				t.Fatalf("explicit new reset was not queued: %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), completedResetSeed) || strings.Contains(w.Body.String(), "Reset-api-fixture9!") {
				t.Fatal("retry response leaked credentials")
			}
		})
	}
}

func TestTOTPResetRetryInitialNetworkFailureAfterRestore(t *testing.T) {
	for _, batch := range []bool{false, true} {
		mode := "single"
		if batch {
			mode = "batch"
		}
		t.Run(mode, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
			m.SetCredentialStore(vault)
			j := importTOTPActionFixture(t, m, "initial-timeout", true)
			if _, err := m.QueueOperation(j.ID, "reset-2fa", map[string]any{"reloginAfterTotp": true}); err != nil {
				t.Fatal(err)
			}
			if err := m.StartTOTPWorkflowStep(context.Background(), j.ID, "totp"); err != nil {
				t.Fatal(err)
			}
			failure := errors.Join(errors.New("request timed out"), context.DeadlineExceeded)
			if err := m.FinishTOTPWorkflowContext(context.Background(), j.ID, failure); err != nil {
				t.Fatal(err)
			}
			m.Emit(jobs.JobEvent{JobID: j.ID, Stage: "legacy_session_start", Status: jobs.StatusFailed, Prompt: "request timed out"})
			m = restoreCompletedTOTPFixtureManager(t, s, vault)
			current, _ := m.Get(j.ID)
			if current.Status != jobs.StatusFailed || current.TOTPWorkflowStatus != "failed" || current.TOTPResetPhase != "initial" || current.TOTPRecoveryPending || current.TOTPCredentialInvalidated || !current.HasTOTPKey || s.canSecurityOperation(current) || m.QueuePosition(j.ID) != 0 {
				t.Fatal("initial failure did not retain the unmodified factor without an old grant")
			}
			stored, err := m.LoadCredentials(j.ID)
			if err != nil || stored.TOTPSecret != resetAPISeed || stored.TOTPResetPending || stored.PendingTOTPSecret != "" {
				t.Fatal("initial request failure altered the confirmed factor")
			}
			for _, path := range []string{"/api/jobs/" + j.ID, "/api/jobs?pageSize=1"} {
				w := frontendRequest(s, http.MethodGet, path, nil, "test")
				var response map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusOK {
					t.Fatal("initial retry state could not be read")
				}
				row := response
				if path == "/api/jobs?pageSize=1" {
					selection, ok := response["selection"].([]any)
					if !ok || len(selection) != 1 {
						t.Fatal("missing initial retry selection state")
					}
					row = selection[0].(map[string]any)
				}
				if row["canResetTotp"] != true || row["canDownload"] != false || row["totpResetUnavailableReason"] != "" || strings.Contains(w.Body.String(), resetAPISeed) || strings.Contains(w.Body.String(), "Reset-api-fixture9!") {
					t.Fatal("public initial retry eligibility was lost or leaked credentials")
				}
			}
			path := "/api/jobs/" + j.ID + "/setup-2fa"
			body := map[string]any{"resetTotp": true}
			if batch {
				path, body["ids"] = "/api/jobs/setup-2fa-batch", []string{j.ID}
			}
			w := frontendRequest(s, http.MethodPost, path, body, "test")
			queued, _ := m.Get(j.ID)
			if w.Code != http.StatusOK || queued.Status != jobs.StatusQueued || queued.Operation != "reset-2fa" || queued.TOTPWorkflowStatus != "pending" || queued.TOTPResetPhase != "initial" || m.QueuePosition(j.ID) != 1 {
				t.Fatalf("manual reset after initial failure was not queued: %d", w.Code)
			}
			stored, err = vault.Load(j.Email)
			if err != nil || stored.TOTPSecret != resetAPISeed || stored.TOTPResetPending || stored.PendingTOTPSecret != "" {
				t.Fatal("manual reset retry lost the unmodified factor")
			}
			// A second request cannot queue a duplicate or replay a mutation.
			w = frontendRequest(s, http.MethodPost, path, body, "test")
			if w.Code != http.StatusConflict || m.QueuePosition(j.ID) != 1 {
				t.Fatal("repeated initial retry added duplicate queued work")
			}
		})
	}
}

func TestTOTPResetRetryInitialEligibilityStillRechecksPrivateRecovery(t *testing.T) {
	for _, evidence := range []string{"vault_pending", "other_row_journal"} {
		t.Run(evidence, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
			m.SetCredentialStore(vault)
			j := importTOTPActionFixture(t, m, "initial-private-recovery", true)
			if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			if err := m.StartTOTPWorkflowStep(context.Background(), j.ID, "totp"); err != nil {
				t.Fatal(err)
			}
			if err := m.FinishTOTPWorkflowContext(context.Background(), j.ID, context.DeadlineExceeded); err != nil {
				t.Fatal(err)
			}
			m.Emit(jobs.JobEvent{JobID: j.ID, Stage: "legacy_session_start", Status: jobs.StatusFailed})
			m = restoreCompletedTOTPFixtureManager(t, s, vault)
			current, _ := m.Get(j.ID)
			if s.publicJob(current)["canResetTotp"] != true {
				t.Fatal("fixture did not qualify before new private recovery evidence")
			}
			switch evidence {
			case "vault_pending":
				stored, err := vault.Load(j.Email)
				if err != nil {
					t.Fatal(err)
				}
				stored.TOTPResetPending = true
				if err := vault.Save(j.Email, stored); err != nil {
					t.Fatal(err)
				}
			case "other_row_journal":
				peer, err := m.Submit(jobs.Spec{ID: "same-account-history", Email: j.Email, ImportOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				peerRoot := filepath.Join(s.cfg.OutputRoot, peer.ID)
				if err := os.MkdirAll(peerRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(peerRoot, "totp-setup-result.json"), []byte(`{"version":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/setup-2fa", map[string]any{"resetTotp": true}, "test")
			current, _ = m.Get(j.ID)
			if w.Code != http.StatusConflict || current.Status != jobs.StatusFailed || current.TOTPResetPhase != "initial" || m.QueuePosition(j.ID) != 0 {
				t.Fatal("initial retry bypassed current private recovery evidence")
			}
		})
	}
}

func TestTOTPResetRetryBatchIncludesCompletedSavedFactors(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	ids := []string{}
	for _, logout := range []string{"failed", "unknown", "succeeded"} {
		j := queueCompletedTOTPResetFixture(t, m, vault, "batch-saved-"+logout, logout)
		ids = append(ids, j.ID)
	}
	m = restoreCompletedTOTPFixtureManager(t, s, vault)
	for _, id := range ids {
		assertCompletedTOTPResetRetryState(t, s, m, id, completedResetSeed)
	}
	busy := importTOTPActionFixture(t, m, "batch-busy", true)
	if _, err := m.QueueOperation(busy.ID, "relogin", nil); err != nil {
		t.Fatal(err)
	}
	recovering := importTOTPActionFixture(t, m, "batch-recovering", true)
	if _, err := m.QueueOperation(recovering.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginTOTPReset(context.Background(), recovering.ID); err != nil {
		t.Fatal(err)
	}
	unknown := importTOTPActionFixture(t, m, "batch-unconfirmed", false)
	all := append(append([]string{}, ids...), busy.ID, recovering.ID, unknown.ID)
	w := frontendRequest(s, http.MethodGet, "/api/jobs?pageSize=1", nil, "test")
	var page struct {
		Jobs      []map[string]any `json:"jobs"`
		Selection []map[string]any `json:"selection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || len(page.Jobs) != 1 || len(page.Selection) != len(all) {
		t.Fatalf("read cross-page retry eligibility: %d %s", w.Code, w.Body.String())
	}
	eligible := map[string]bool{}
	for _, row := range page.Selection {
		id, ok := row["id"].(string)
		if !ok {
			t.Fatal("selection row has no account ID")
		}
		if row["canResetTotp"] == true {
			eligible[id] = true
		}
	}
	if len(eligible) != len(ids) {
		t.Fatal("cross-page batch selection advertised an unsafe or missed saved reset")
	}
	for _, id := range ids {
		if !eligible[id] {
			t.Fatal("cross-page batch selection omitted retry eligibility", id)
		}
	}
	w = frontendRequest(s, http.MethodPost, "/api/jobs/setup-2fa-batch", map[string]any{"ids": all, "resetTotp": true}, "test")
	var result struct {
		Started int              `json:"started"`
		Skipped int              `json:"skipped"`
		Jobs    []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Started != 3 || result.Skipped != 3 || len(result.Jobs) != 3 {
		t.Fatalf("batch retry chose incorrect accounts: %d %s", w.Code, w.Body.String())
	}
	for _, id := range ids {
		j, _ := m.Get(id)
		if j.Operation != "reset-2fa" || j.Status != jobs.StatusQueued || j.TOTPWorkflowStatus != "pending" {
			t.Fatal("eligible batch account was not queued", id)
		}
	}
	stillBusy, _ := m.Get(busy.ID)
	stillRecovering, _ := m.Get(recovering.ID)
	stillUnknown, _ := m.Get(unknown.ID)
	if stillBusy.Operation != "relogin" || !stillRecovering.TOTPRecoveryPending || stillUnknown.Status != jobs.StatusImported {
		t.Fatal("batch retry changed a blocked account")
	}
}

func TestTOTPResetRetryEligibilityAllowsOnlySafeTerminalResets(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	for _, terminal := range []struct {
		status   string
		workflow string
	}{
		{jobs.StatusSucceeded, "succeeded"}, {jobs.StatusFailed, "failed"},
		{jobs.StatusCanceled, "interrupted"}, {"reauth_required", "failed"}, {"resume_available", "interrupted"},
	} {
		for _, phase := range []string{"credential_persisted", "initial"} {
			t.Run(terminal.status+"/"+phase, func(t *testing.T) {
				j := &jobs.Job{ID: "terminal-reset", Email: "terminal@example.test", ResetTOTP: true, TOTPResetPhase: phase, HasTOTPKey: true, TOTPKnownEnabled: true, Status: terminal.status, TOTPWorkflowStatus: terminal.workflow}
				if phase == "initial" {
					j.TOTPWorkflowStep, j.TOTPResetStatus = "totp", "failed"
				}
				if !canRestartCompletedTOTPReset(j) || s.publicJob(j)["canResetTotp"] != true {
					t.Fatal("safe terminal reset cannot start an explicit new reset")
				}
			})
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*jobs.Job)
	}{
		{"queued", func(j *jobs.Job) { j.Status = jobs.StatusQueued }},
		{"running", func(j *jobs.Job) { j.Status = jobs.StatusRunning }},
		{"workflow-pending", func(j *jobs.Job) { j.TOTPWorkflowStatus = "pending" }},
		{"workflow-running", func(j *jobs.Job) { j.TOTPWorkflowStatus = "running" }},
		{"workflow-unknown", func(j *jobs.Job) { j.TOTPWorkflowStatus = "unknown" }},
		{"workflow-unset", func(j *jobs.Job) { j.TOTPWorkflowStatus = "" }},
		{"recovery-pending", func(j *jobs.Job) { j.TOTPRecoveryPending = true }},
		{"credential-invalidated", func(j *jobs.Job) { j.TOTPCredentialInvalidated = true }},
		{"new-key-unsaved", func(j *jobs.Job) { j.HasTOTPKey = false }},
		{"phase-uncommitted", func(j *jobs.Job) { j.TOTPResetPhase = "activated" }},
		{"phase-recovery", func(j *jobs.Job) { j.TOTPResetPhase = "recovery_required" }},
		{"phase-disabling", func(j *jobs.Job) { j.TOTPResetPhase = "disable_intent" }},
		{"phase-disabled", func(j *jobs.Job) { j.TOTPResetPhase = "disabled" }},
		{"phase-unknown", func(j *jobs.Job) { j.TOTPResetPhase = "unknown" }},
		{"initial-ambiguous-status", func(j *jobs.Job) {
			j.TOTPResetPhase, j.TOTPResetStatus, j.TOTPWorkflowStep = "initial", "unknown", "totp"
		}},
		{"initial-later-step", func(j *jobs.Job) {
			j.TOTPResetPhase, j.TOTPResetStatus, j.TOTPWorkflowStep = "initial", "failed", "logout"
		}},
		{"not-a-reset", func(j *jobs.Job) { j.ResetTOTP = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &jobs.Job{ID: "blocked-reset", Email: "blocked@example.test", ResetTOTP: true, TOTPResetPhase: "credential_persisted", HasTOTPKey: true, TOTPKnownEnabled: true, Status: jobs.StatusFailed, TOTPWorkflowStatus: "failed"}
			tc.change(j)
			if canRestartCompletedTOTPReset(j) || s.publicJob(j)["canResetTotp"] != false || s.totpUnavailableReason(j, true) == "" {
				t.Fatal("unsafe or unfinished reset became eligible")
			}
		})
	}
}

func TestTOTPResetRetryRepeatedManualResetsKeepLatestSavedFactor(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	j := importTOTPActionFixture(t, m, "repeated-reset", true)
	for _, seed := range []string{completedResetSeed, "MFRGGZDFMZTWQ2LK", "ONSWG4TFOQWWK4ZB"} {
		w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/setup-2fa", map[string]any{"resetTotp": true, "logoutAllDevicesAfterTotp": true}, "test")
		if w.Code != http.StatusOK {
			t.Fatalf("explicit repeated reset was rejected: %d %s", w.Code, w.Body.String())
		}
		completeQueuedTOTPResetFixture(t, m, vault, j.ID, seed, "failed")
		committed, _ := m.Get(j.ID)
		if committed.Input["totpSecret"] != seed {
			t.Fatal("in-memory login input retained an older factor")
		}
		m = restoreCompletedTOTPFixtureManager(t, s, vault)
		assertCompletedTOTPResetRetryState(t, s, m, j.ID, seed)
		stored, err := m.LoadCredentials(j.ID)
		if err != nil || stored.TOTPSecret != seed || stored.TOTPResetPending {
			t.Fatal("repeated reset did not retain the latest confirmed factor")
		}
	}
}
