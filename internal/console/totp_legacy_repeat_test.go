package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func legacyCompletedResetFixture(t *testing.T) (*Server, *jobs.Manager, *monitorMemoryVault, string) {
	t.Helper()
	s, _ := newMonitorTestServer(t)
	id, email := "legacy-completed-reset", "legacy-completed-reset@example.test"
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{
		email: {Password: "Synthetic-reset-password9!", TOTPSecret: resetAPISeed},
	}}
	metadata := map[string]any{
		"version": 1,
		"jobs": []any{map[string]any{
			"id": id, "email": email, "operation": "reset-2fa", "status": jobs.StatusSucceeded,
			"operation_status": "succeeded", "stage": "complete", "reset_totp": true,
			"totp_reset_phase": "credential_persisted", "totp_reset_status": "activated",
			"has_totp_key": true, "totp_known_enabled": true,
			"totp_workflow_status": "succeeded", "totp_workflow_step": "upload",
			"logout_all_devices_after_totp": true, "logout_all_devices_status": "succeeded",
			"relogin_after_totp": true, "totp_relogin_status": "succeeded",
			"upload_after_totp": true, "totp_upload_status": "succeeded",
		}},
	}
	b, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	m := restoreCompletedTOTPFixtureManager(t, s, vault)
	return s, m, vault, id
}

func TestLegacyCompletedTOTPResetCanStartRepeatedExplicitResets(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			s, m, vault, id := legacyCompletedResetFixture(t)
			legacy, _ := m.Get(id)
			if s.publicJob(legacy)["canResetTotp"] != true || legacy.TOTPResetCommitted || legacy.TOTPActivationVerified || legacy.TOTPResetOperationID != "" {
				t.Fatal("old completed record was blocked or given invented commit evidence")
			}
			for round, seed := range []string{completedResetSeed, "MFRGGZDFMZTWQ2LK", "ONSWG4TFOQWWK4ZB", "GEZDGNBVGY3TQOJQ"} {
				body := map[string]any{"resetTotp": true, "logoutAllDevicesAfterTotp": true, "totpIntentId": fmt.Sprintf("legacy-repeat-intent-%d", round)}
				path := "/api/jobs/" + id + "/setup-2fa"
				if batch {
					path, body["ids"] = "/api/jobs/setup-2fa-batch", []string{id}
				}
				for attempt := 0; attempt < 2; attempt++ {
					w := frontendRequest(s, http.MethodPost, path, body, "test")
					queued, _ := m.Get(id)
					if w.Code != http.StatusOK || m.QueuePosition(id) != 1 || queued.Status != jobs.StatusQueued || queued.TOTPResetCommitted || queued.TOTPActivationVerified || queued.TOTPResetOperationID != "" {
						t.Fatalf("explicit reset admission/replay failed: round=%d attempt=%d HTTP=%d", round, attempt, w.Code)
					}
				}
				completeQueuedTOTPResetFixture(t, s.cfg.OutputRoot, m, vault, id, seed, "succeeded")
				m = restoreCompletedTOTPFixtureManager(t, s, vault)
				finished, _ := m.Get(id)
				current, err := m.LoadCredentials(id)
				if err != nil || current.TOTPSecret != seed || current.TOTPResetPending || !finished.TOTPResetCommitted || !finished.TOTPActivationVerified || finished.TOTPResetOperationID == "" || s.publicJob(finished)["canResetTotp"] != true || m.QueuePosition(id) != 0 {
					t.Fatalf("completed reset cannot start next explicit round: round=%d", round)
				}
			}
		})
	}
}

func TestLegacyCompletedTOTPResetDoesNotBypassUnfinishedState(t *testing.T) {
	scenarios := []struct {
		name   string
		change func(*jobs.Job)
	}{
		{"modern-uncommitted", func(j *jobs.Job) { j.TOTPResetOperationID = "current-operation" }},
		{"modern-activation-uncommitted", func(j *jobs.Job) { j.TOTPActivationVerified = true }},
		{"queued", func(j *jobs.Job) { j.Status = jobs.StatusQueued }},
		{"running", func(j *jobs.Job) { j.Status = jobs.StatusRunning }},
		{"unknown", func(j *jobs.Job) { j.TOTPResetStatus = "unknown" }},
		{"workflow-running", func(j *jobs.Job) { j.TOTPWorkflowStatus = "running" }},
		{"missing-key", func(j *jobs.Job) { j.HasTOTPKey = false }},
		{"pending", func(j *jobs.Job) { j.TOTPRecoveryPending = true }},
		{"invalidated", func(j *jobs.Job) { j.TOTPCredentialInvalidated = true }},
		{"logout-unknown", func(j *jobs.Job) { j.LogoutAllDevicesStatus = "unknown" }},
		{"relogin-pending", func(j *jobs.Job) { j.TOTPReloginStatus = "pending" }},
		{"upload-pending", func(j *jobs.Job) { j.TOTPUploadStatus = "pending" }},
		{"save-error", func(j *jobs.Job) { j.TOTPSetupError = "synthetic save failure" }},
	}
	s, m, _, id := legacyCompletedResetFixture(t)
	base, _ := m.Get(id)
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			j := *base
			scenario.change(&j)
			if canRestartCompletedTOTPReset(&j) || s.publicJob(&j)["canResetTotp"] != false {
				t.Fatal("unfinished or contradictory state was accepted as an old completed reset")
			}
		})
	}
	for _, blockedBy := range []string{"private-journal", "vault-pending"} {
		t.Run(blockedBy, func(t *testing.T) {
			s, m, vault, id := legacyCompletedResetFixture(t)
			before, _ := m.Get(id)
			if blockedBy == "private-journal" {
				dir := filepath.Join(s.cfg.OutputRoot, id)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "totp-setup-result.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				c := vault.values[before.Email]
				c.TOTPResetPending = true
				vault.values[before.Email] = c
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/"+id+"/setup-2fa", map[string]any{"resetTotp": true, "totpIntentId": "blocked-legacy-intent"}, "test")
			after, _ := m.Get(id)
			if w.Code != http.StatusConflict || m.QueuePosition(id) != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("legacy admission bypassed private evidence or changed a blocked account")
			}
		})
	}
}
