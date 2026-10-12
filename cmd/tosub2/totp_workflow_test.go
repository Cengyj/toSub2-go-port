package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestTOTPWorkflowOrderAndFailureBoundaries(t *testing.T) {
	for _, scenario := range []string{"success", "logout-unknown", "logout-http400", "logout-english", "logout-secret", "factor-failed", "factor-timeout", "factor-child-canceled", "canceled-after-logout", "canceled-during-logout", "timeout-during-logout", "relogin-failed", "missing-fresh-result", "upload-failed", "pending-factor", "bad-config"} {
		t.Run(scenario, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			m := jobs.NewManager(context.Background(), 1, st, nil)
			m.SetCredentialStore(vault)
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			job, err := m.Submit(jobs.Spec{ID: "workflow", Email: "workflow@example.test", ImportOnly: true, Input: map[string]any{"password": "fixture-password", "totpSecret": "OLDFACTOREVIDENCE"}})
			if err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{"baseUrl": "https://sub2api.example.test", "adminApiKey": "nested-canary-admin-key", "groupIds": []string{"11"}, "priority": "0"}
			if scenario == "bad-config" {
				cfg["baseUrl"] = "invalid"
			}
			job, err = m.QueueOperation(job.ID, "reset-2fa", map[string]any{"resetTotp": true, "logoutAllDevicesAfterTotp": true, "reloginAfterTotp": true, "uploadAfterTotp": true, "sub2apiConfig": cfg})
			if err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(job.ID, true, "old grant available")
			ctx, cancel := context.WithCancel(t.Context())
			if scenario == "timeout-during-logout" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
			}
			defer cancel()
			calls := []string{}
			operation := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				calls = append(calls, active.Operation)
				if active.Operation == "reset-2fa" {
					if scenario == "factor-timeout" {
						return context.DeadlineExceeded
					}
					if scenario == "factor-child-canceled" {
						return context.Canceled
					}
					if scenario == "factor-failed" {
						return errors.New("factor operation failed")
					}
					if err := m.BeginTOTPReset(ctx, active.ID); err != nil {
						return err
					}
					c, _ := vault.Load(active.Email)
					c.TOTPSecret, c.TOTPResetPending = "NEWFACTOREVIDENCE", false
					if err := vault.Save(active.Email, c); err != nil {
						return err
					}
					if err := m.CommitTOTPReset(ctx, active.ID, c); err != nil {
						return err
					}
					if err := m.StartTOTPWorkflowStep(ctx, active.ID, "logout"); err != nil {
						return err
					}
					calls = append(calls, "logout")
					state, _ := m.Get(active.ID)
					if state.TOTPWorkflowStep != "logout" || state.TOTPResetPhase != "credential_persisted" {
						t.Error("logout before confirmed credential persistence")
					}
					status := "succeeded"
					if scenario == "logout-unknown" || scenario == "canceled-during-logout" || scenario == "timeout-during-logout" {
						status = "unknown"
					}
					reason := map[string]string{
						"logout-http400": "退出所有设备请求被拒绝（HTTP 400；错误标识 invalid_request）",
						"logout-english": "Logout request returned HTTP 400; it will not be retried.",
						"logout-secret":  "退出所有设备请求被拒绝（HTTP 400） fixture-password NEWFACTOREVIDENCE nested-canary-admin-key",
					}[scenario]
					if reason != "" {
						status = "failed"
					}
					if err := m.UpdateTOTPResetResult(active.ID, map[string]any{"logout_all_devices_requested": true, "logout_all_devices_status": status, "logout_all_devices_error": reason}, nil); err != nil {
						return err
					}
					if scenario == "pending-factor" {
						c.PendingTOTPSecret = "UNVERIFIEDFACTOR"
						if err := vault.Save(active.Email, c); err != nil {
							return err
						}
					}
					if scenario == "canceled-after-logout" || scenario == "canceled-during-logout" {
						cancel()
					}
					if scenario == "timeout-during-logout" {
						<-ctx.Done()
					}
					return nil
				}
				if active.Operation != "relogin" {
					t.Fatal("unexpected operation", active.Operation)
				}
				values, _ := m.InputCopy(active.ID)
				state, _ := m.Get(active.ID)
				if values["totpSecret"] != "NEWFACTOREVIDENCE" || state.LogoutAllDevicesStatus != "succeeded" || state.ResultSaved {
					t.Error("relogin used stale secret/output or preceded logout")
				}
				if scenario == "relogin-failed" {
					return errors.New("fixture authorization failed")
				}
				if scenario != "missing-fresh-result" {
					m.UpdateResult(active.ID, true, "new grant saved")
				}
				return nil
			}
			upload := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				calls = append(calls, "upload")
				state, _ := m.Get(active.ID)
				if state.TOTPReloginStatus != "succeeded" || !state.ResultSaved || state.TOTPWorkflowStep != "upload" {
					t.Error("upload before new authorization persisted")
				}
				if scenario == "upload-failed" {
					return errors.New("remote failure nested-canary-admin-key")
				}
				return nil
			}
			err = makeTOTPWorkflowRunner(m, vault, operation, upload)(ctx, job, func(jobs.JobEvent) {})
			state, _ := m.Get(job.ID)
			want := []string{"reset-2fa", "logout"}
			switch scenario {
			case "bad-config":
				want = []string{}
			case "factor-failed", "factor-timeout", "factor-child-canceled":
				want = []string{"reset-2fa"}
			case "relogin-failed", "missing-fresh-result":
				want = append(want, "relogin")
			case "success", "upload-failed":
				want = append(want, "relogin", "upload")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("requests %v, want %v", calls, want)
			}
			if (err == nil) != (scenario == "success") {
				t.Fatalf("unexpected workflow error %v", err)
			}
			if scenario == "success" && (state.TOTPWorkflowStatus != "succeeded" || state.TOTPReloginStatus != "succeeded" || state.TOTPUploadStatus != "succeeded") {
				t.Fatal("success not fully acknowledged")
			}
			if scenario == "upload-failed" && state.TOTPUploadStatus != "unknown" {
				t.Fatal("uncertain upload treated as safely replayable")
			}
			if scenario == "factor-timeout" || scenario == "factor-child-canceled" {
				cause := error(context.DeadlineExceeded)
				if scenario == "factor-child-canceled" {
					cause = context.Canceled
				}
				if ctx.Err() != nil || !errors.Is(err, cause) || state.TOTPWorkflowStatus != "failed" || state.TOTPResetPhase != "initial" || state.TOTPRecoveryPending || state.TOTPCredentialInvalidated || state.TOTPReloginStatus != "skipped" || state.TOTPUploadStatus != "skipped" {
					t.Fatal("request failure was mislabeled as job cancellation or advanced later steps")
				}
			}
			if strings.HasPrefix(scenario, "logout-") && scenario != "logout-unknown" {
				if !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(state.TOTPWorkflowError, "HTTP 400") || !strings.Contains(err.Error(), "新 2FA 已保存") {
					t.Fatal("confirmed credential persistence or concrete logout failure hidden")
				}
				if scenario == "logout-http400" && !strings.Contains(err.Error(), "invalid_request") {
					t.Fatal("safe machine diagnostic hidden")
				}
				if scenario == "logout-english" && !strings.Contains(err.Error(), "Logout request returned HTTP 400") {
					t.Fatal("legacy English logout diagnostic hidden")
				}
			}
			if scenario == "canceled-after-logout" || scenario == "canceled-during-logout" || scenario == "timeout-during-logout" {
				cause := error(context.Canceled)
				if scenario == "timeout-during-logout" {
					cause = context.DeadlineExceeded
				}
				if !errors.Is(err, cause) || state.TOTPWorkflowStatus != "interrupted" || state.TOTPReloginStatus != "skipped" || state.TOTPUploadStatus != "skipped" {
					t.Fatal("cancellation/deadline overwritten by generic logout failure")
				}
			}
			values, _ := m.InputCopy(job.ID)
			if values["sub2apiConfig"] != nil {
				t.Fatal("integration key retained after workflow")
			}
			meta, _ := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
			for _, secret := range []string{"nested-canary-admin-key", "OLDFACTOREVIDENCE", "NEWFACTOREVIDENCE", "fixture-password"} {
				if strings.Contains(string(meta), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
					t.Fatal("secret disclosed in workflow metadata/error")
				}
			}
			// Calling with the latest terminal snapshot cannot replay a factor,
			// logout or upload, even before a process restart.
			before := len(calls)
			if repeat := makeTOTPWorkflowRunner(m, vault, operation, upload)(t.Context(), state, func(jobs.JobEvent) {}); repeat == nil || len(calls) != before {
				t.Fatal("terminal workflow replayed")
			}
		})
	}
}
