package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestFreshTOTPResetAfterNetworkFailureWithoutOldSession(t *testing.T) {
	for _, operation := range []string{"reset-2fa", "full", "relogin"} {
		for _, seed := range []bool{false, true} {
			for _, batch := range []bool{false, true} {
				name := operation + "/known-only"
				if seed {
					name = operation + "/with-seed"
				}
				if batch {
					name += "/batch"
				}
				t.Run(name, func(t *testing.T) {
					s, m := newMonitorTestServer(t)
					vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
					m.SetCredentialStore(vault)
					j := importTOTPActionFixture(t, m, "network-failure", seed)
					m.UpdateTOTPSetup(j.ID, "", "", "", true)
					before, err := vault.Load(j.Email)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := m.QueueOperation(j.ID, operation, nil); err != nil {
						t.Fatal(err)
					}
					if operation == "reset-2fa" {
						if err := m.StartTOTPWorkflowStep(context.Background(), j.ID, "totp"); err != nil {
							t.Fatal(err)
						}
						if err := m.FinishTOTPWorkflowContext(context.Background(), j.ID, errors.New("network is unreachable")); err != nil {
							t.Fatal(err)
						}
					}
					m.Emit(jobs.JobEvent{JobID: j.ID, Stage: "legacy_session_start", Status: jobs.StatusFailed, Prompt: "network is unreachable"})
					m = restoreCompletedTOTPFixtureManager(t, s, vault)
					current, _ := m.Get(j.ID)
					if s.canSecurityOperation(current) || current.Status != jobs.StatusFailed || current.HasTOTPKey != seed || !current.TOTPKnownEnabled {
						t.Fatal("fixture unexpectedly retained an old session or lost the known factor")
					}
					for _, path := range []string{"/api/jobs/" + j.ID, "/api/jobs?pageSize=1"} {
						w := frontendRequest(s, http.MethodGet, path, nil, "test")
						var response map[string]any
						if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusOK {
							t.Fatal("cannot read fresh reset eligibility", err)
						}
						row := response
						if path == "/api/jobs?pageSize=1" {
							row = response["selection"].([]any)[0].(map[string]any)
						}
						if row["canResetTotp"] != true || row["canSetupTotp"] != false || row["totpResetUnavailableReason"] != "" {
							t.Fatal("safe fresh reset was not advertised consistently")
						}
					}
					path := "/api/jobs/" + j.ID + "/setup-2fa"
					body := map[string]any{"resetTotp": true}
					if batch {
						path, body["ids"] = "/api/jobs/setup-2fa-batch", []string{j.ID}
					}
					w := frontendRequest(s, http.MethodPost, path, body, "test")
					queued, _ := m.Get(j.ID)
					if w.Code != http.StatusOK || queued.Status != jobs.StatusQueued || queued.Operation != "reset-2fa" || queued.TOTPResetPhase != "initial" || m.QueuePosition(j.ID) != 1 {
						t.Fatalf("fresh reset was not queued: %d %s", w.Code, w.Body.String())
					}
					after, err := vault.Load(j.Email)
					if err != nil || after.TOTPSecret != before.TOTPSecret || after.TOTPResetPending {
						t.Fatal("queuing a fresh reset modified the existing factor")
					}
					w = frontendRequest(s, http.MethodPost, path, body, "test")
					if w.Code != http.StatusConflict || m.QueuePosition(j.ID) != 1 {
						t.Fatal("repeated click added a second reset")
					}
				})
			}
		}
	}
}

func TestFreshTOTPResetNeverOverridesUncertainMutationOrBusyWorkflow(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	for _, status := range []string{jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCanceled, "interrupted", "reauth_required", "resume_available"} {
		j := &jobs.Job{ID: "fresh-reset", Email: "fresh@example.test", Operation: "relogin", Status: status, TOTPKnownEnabled: true}
		if !canStartFreshTOTPReset(j) || s.publicJob(j)["canResetTotp"] != true {
			t.Fatal("safe terminal account requires an old grant", status)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*jobs.Job)
	}{
		{"queued", func(j *jobs.Job) { j.Status = jobs.StatusQueued }},
		{"running", func(j *jobs.Job) { j.Status = jobs.StatusRunning }},
		{"waiting", func(j *jobs.Job) { j.Status = jobs.StatusWaitingInput }},
		{"factor-unconfirmed", func(j *jobs.Job) { j.TOTPKnownEnabled = false }},
		{"recovering", func(j *jobs.Job) { j.TOTPRecoveryPending = true }},
		{"invalidated", func(j *jobs.Job) { j.TOTPCredentialInvalidated = true }},
		{"unknown-reset-status", func(j *jobs.Job) { j.TOTPResetStatus = "unknown" }},
		{"disable-intent", func(j *jobs.Job) { j.TOTPResetPhase = "disable_intent" }},
		{"enrolled", func(j *jobs.Job) { j.TOTPResetPhase = "enrolled" }},
		{"activation-intent", func(j *jobs.Job) { j.TOTPResetPhase = "activation_intent" }},
		{"recovery-required", func(j *jobs.Job) { j.TOTPResetPhase = "recovery_required" }},
		{"workflow-pending", func(j *jobs.Job) { j.TOTPWorkflowStatus = "pending" }},
		{"workflow-running", func(j *jobs.Job) { j.TOTPWorkflowStatus = "running" }},
		{"workflow-unknown", func(j *jobs.Job) { j.TOTPWorkflowStatus = "unknown" }},
		{"unfinished-setup", func(j *jobs.Job) { j.Operation = "setup-2fa" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &jobs.Job{ID: "fresh-reset", Email: "fresh@example.test", Operation: "relogin", Status: jobs.StatusFailed, TOTPKnownEnabled: true}
			tc.edit(j)
			if canStartFreshTOTPReset(j) || s.publicJob(j)["canResetTotp"] != false {
				t.Fatal("unsafe reset was made available")
			}
		})
	}
	// An authorization retained from an earlier operation must not bypass
	// uncertain mutation evidence in incomplete restored metadata.
	for _, phase := range []string{"disable_intent", "enrolled", "activation_intent", "recovery_required"} {
		j := &jobs.Job{ID: "old-grant", Email: "old@example.test", Operation: "relogin", Status: jobs.StatusSucceeded, ResultSaved: true, TOTPKnownEnabled: true, TOTPResetPhase: phase}
		if !s.canSecurityOperation(j) || s.publicJob(j)["canResetTotp"] != false {
			t.Fatal("old grant overrode uncertain mutation evidence", phase)
		}
	}
	for _, workflow := range []string{"pending", "running", "unknown"} {
		j := &jobs.Job{ID: "unfinished-workflow", Email: "old@example.test", Operation: "setup-2fa", Status: jobs.StatusSucceeded, ResultSaved: true, TOTPKnownEnabled: true, TOTPWorkflowStatus: workflow}
		if !s.canSecurityOperation(j) || s.publicJob(j)["canResetTotp"] != false {
			t.Fatal("old grant overrode unfinished security workflow", workflow)
		}
	}
}
