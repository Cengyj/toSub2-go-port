package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/store"
)

func queueReloginUploadFixture(t *testing.T, m *Manager, job *Job) *Job {
	t.Helper()
	j, err := m.QueueOperation(job.ID, "relogin", map[string]any{
		"uploadAfterRelogin": true,
		"sub2apiConfig":      map[string]any{"baseUrl": "https://sub2api.example.test", "adminApiKey": "relogin-upload-canary-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestReloginUploadRequiresFreshAuthorizationAndErasesPrivateConfig(t *testing.T) {
	m, _, job, _ := resetManagerFixture(t)
	m.UpdateResult(job.ID, true, "old authorization available")
	job = queueReloginUploadFixture(t, m, job)
	if err := m.StartReloginUpload(t.Context(), job.ID); err == nil {
		t.Fatal("old saved authorization allowed automatic upload")
	}
	m.UpdateResult(job.ID, true, "new authorization saved")
	if err := m.StartReloginUpload(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.StartReloginUpload(t.Context(), job.ID); err == nil {
		t.Fatal("remote sync could be started twice")
	}
	if err := m.FinishReloginUpload(job.ID, errors.New("lost response relogin-upload-canary-key"), true); err != nil {
		t.Fatal(err)
	}
	state, _ := m.Get(job.ID)
	if state.ReloginUploadStatus != "unknown" || !state.ResultSaved || !state.ReloginAuthorizationSaved || strings.Contains(state.ReloginUploadError, "relogin-upload-canary-key") {
		t.Fatal("sync uncertainty lost new grant or disclosed integration key")
	}
	values, _ := m.InputCopy(job.ID)
	if values["sub2apiConfig"] != nil {
		t.Fatal("integration configuration retained after sync")
	}
	data, err := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
	if err != nil || strings.Contains(string(data), "relogin-upload-canary-key") {
		t.Fatal("integration key reached metadata", err)
	}
	state, err = m.QueueOperation(job.ID, "relogin", nil)
	if err != nil || state.UploadAfterRelogin || state.ReloginAuthorizationSaved || state.ReloginUploadStatus != "" || state.ReloginUploadError != "" {
		t.Fatal("independent operation retained previous sync result", err)
	}
}

func TestReloginUploadRestartNeverReplaysQueuedOrUncertainSync(t *testing.T) {
	for _, step := range []string{"queued", "authorized", "upload"} {
		t.Run(step, func(t *testing.T) {
			m, vault, job, _ := resetManagerFixture(t)
			job = queueReloginUploadFixture(t, m, job)
			if step != "queued" {
				dir, err := store.New(filepath.Join(m.store.Root, job.ID))
				if err != nil {
					t.Fatal(err)
				}
				if err := dir.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": job.Email, "access_token": "new-grant"}}}}); err != nil {
					t.Fatal(err)
				}
				m.Emit(JobEvent{JobID: job.ID, Stage: "authorization", Status: StatusRunning})
				m.UpdateResult(job.ID, true, "new authorization saved")
			}
			if step == "upload" {
				if err := m.StartReloginUpload(t.Context(), job.ID); err != nil {
					t.Fatal(err)
				}
			}
			restored := NewManager(context.Background(), 1, m.store, nil)
			restored.SetCredentialStore(vault)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			state, _ := restored.Get(job.ID)
			want := "skipped"
			if step == "upload" {
				want = "unknown"
			}
			if state.ReloginUploadStatus != want || state.Status == StatusQueued || len(restored.pending) != 0 || state.Input["sub2apiConfig"] != nil {
				t.Fatal("restart allowed replay or retained integration key", state)
			}
			if step != "queued" && (!state.ResultSaved || !state.ReloginAuthorizationSaved) {
				t.Fatal("restart lost new saved authorization")
			}
			if step != "queued" && (state.OperationStatus != "succeeded" || state.Status != StatusSucceeded) {
				t.Fatal("interrupted optional sync hid the durably saved new authorization")
			}
		})
	}
}

func TestQueuedReloginUploadCancellationDoesNotResumeAfterRestart(t *testing.T) {
	m, vault, job, _ := resetManagerFixture(t)
	job = queueReloginUploadFixture(t, m, job)
	if !m.Cancel(job.ID) {
		t.Fatal("queued relogin could not be canceled")
	}
	state, _ := m.Get(job.ID)
	if state.ReloginUploadStatus != "skipped" || state.Input["sub2apiConfig"] != nil {
		t.Fatal("cancellation left pending sync/private configuration")
	}
	restored := NewManager(context.Background(), 1, m.store, nil)
	restored.SetCredentialStore(vault)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, _ = restored.Get(job.ID)
	if state.OperationStatus != "canceled" || len(restored.pending) != 0 {
		t.Fatal("canceled relogin workflow resumed after restart")
	}
}
