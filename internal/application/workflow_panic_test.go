package application

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func workflowPanicFixture(t *testing.T) (*jobs.Manager, *credentials.MemoryStore, *jobs.Job, string) {
	t.Helper()
	disk, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := jobs.NewManager(t.Context(), 1, disk, nil)
	vault := credentials.NewMemoryStore()
	manager.SetCredentialStore(vault)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	job, err := manager.Submit(jobs.Spec{ID: "workflow-panic", Email: "workflow@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	manager.UpdateResult(job.ID, true, "synthetic existing authorization")
	return manager, vault, job, disk.Root
}

func runAndRecoverWorkflowPanic(t *testing.T, runner jobs.Runner, job *jobs.Job, marker string) {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = runner(t.Context(), job, func(jobs.JobEvent) {})
	}()
	if recovered != marker {
		t.Fatalf("workflow did not propagate the original panic: %T", recovered)
	}
}

func assertWorkflowPanicPrivacy(t *testing.T, manager *jobs.Manager, job *jobs.Job, root, marker string) {
	t.Helper()
	values, _ := manager.InputCopy(job.ID)
	if values["sub2apiConfig"] != nil {
		t.Fatal("workflow retained integration credentials after panic")
	}
	data, err := os.ReadFile(filepath.Join(root, "job-meta.json"))
	if err != nil || strings.Contains(string(data), marker) {
		t.Fatal("workflow exposed panic content in persisted state", err)
	}
}

func TestReloginWorkflowPanicNeverConfirmsOptionalUpload(t *testing.T) {
	for _, stage := range []string{"authorization", "upload"} {
		t.Run(stage, func(t *testing.T) {
			manager, _, initial, root := workflowPanicFixture(t)
			job, err := manager.QueueOperation(initial.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": "https://sub2api.example.test", "adminApiKey": "synthetic-admin-key"}})
			if err != nil {
				t.Fatal(err)
			}
			const marker = "synthetic-private-panic-content"
			uploads := 0
			operation := func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				if stage == "authorization" {
					panic(marker)
				}
				manager.UpdateResult(active.ID, true, "synthetic new authorization")
				return nil
			}
			upload := func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { uploads++; panic(marker) }
			runAndRecoverWorkflowPanic(t, ReloginUploadRunner(manager, operation, upload), job, marker)
			current, _ := manager.Get(job.ID)
			want := "skipped"
			if stage == "upload" {
				want = "unknown"
			}
			if current.ReloginUploadStatus != want || current.ReloginUploadError == "" || !current.ResultSaved || strings.Contains(current.ReloginUploadError, marker) || stage == "authorization" && uploads != 0 || uploads > 1 {
				t.Fatalf("panic produced an incorrect sync outcome: status=%s uploads=%d saved=%t", current.ReloginUploadStatus, uploads, current.ResultSaved)
			}
			assertWorkflowPanicPrivacy(t, manager, current, root, marker)
		})
	}
}

func TestTOTPWorkflowPanicNeverConfirmsSelectedSteps(t *testing.T) {
	for _, stage := range []string{"totp", "relogin", "upload"} {
		t.Run(stage, func(t *testing.T) {
			manager, vault, initial, root := workflowPanicFixture(t)
			job, err := manager.QueueOperation(initial.ID, "setup-2fa", map[string]any{"reloginAfterTotp": stage != "totp", "uploadAfterTotp": stage == "upload", "sub2apiConfig": map[string]any{"baseUrl": "https://sub2api.example.test", "adminApiKey": "synthetic-admin-key"}})
			if err != nil {
				t.Fatal(err)
			}
			const marker = "synthetic-private-panic-content"
			operationCalls, uploads := 0, 0
			operation := func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				operationCalls++
				if active.Operation == "relogin" {
					if stage == "relogin" {
						panic(marker)
					}
					manager.UpdateResult(active.ID, true, "synthetic new authorization")
					return nil
				}
				if stage == "totp" {
					panic(marker)
				}
				saved := credentials.Credentials{TOTPSecret: "JBSWY3DPEHPK3PXP"}
				if err := vault.Save(active.Email, saved); err != nil {
					return err
				}
				manager.UpdateStoredCredentials(active.ID, saved)
				return nil
			}
			upload := func(context.Context, *jobs.Job, func(jobs.JobEvent)) error { uploads++; panic(marker) }
			runAndRecoverWorkflowPanic(t, TOTPWorkflowRunner(manager, vault, operation, upload), job, marker)
			current, _ := manager.Get(job.ID)
			if current.TOTPWorkflowStatus != "failed" || current.TOTPWorkflowError == "" || strings.Contains(current.TOTPWorkflowError, marker) || stage == "totp" && (operationCalls != 1 || uploads != 0) || stage == "relogin" && uploads != 0 || stage == "upload" && (uploads != 1 || current.TOTPUploadStatus != "unknown" || !current.ResultSaved) {
				t.Fatalf("panic produced an incorrect 2FA outcome: workflow=%s upload=%s operations=%d uploads=%d", current.TOTPWorkflowStatus, current.TOTPUploadStatus, operationCalls, uploads)
			}
			assertWorkflowPanicPrivacy(t, manager, current, root, marker)
		})
	}
}
