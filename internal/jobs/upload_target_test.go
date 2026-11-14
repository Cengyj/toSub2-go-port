package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCPAWorkflowPreservesTargetAndErasesSecretsOnUnknownOutcome(t *testing.T) {
	m, vault, job, _ := resetManagerFixture(t)
	job, err := m.QueueOperation(job.ID, "relogin", map[string]any{
		"uploadAfterRelogin": true, "uploadTarget": "cpa",
		"cpaConfig":     map[string]any{"baseUrl": "https://cpa.example.test", "adminApiKey": "cpa-private-canary"},
		"sub2apiConfig": map[string]any{"adminApiKey": "unused-sub2-canary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.UploadTarget != UploadTargetCPA || job.Input["sub2apiConfig"] != nil {
		t.Fatal("destination or private configuration selection was lost")
	}
	m.UpdateResult(job.ID, true, "new authorization saved")
	if err := m.StartReloginUpload(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishReloginUpload(job.ID, errors.New("lost response cpa-private-canary"), true); err != nil {
		t.Fatal(err)
	}
	state, _ := m.Get(job.ID)
	if state.ReloginUploadStatus != "unknown" || !state.ResultSaved || state.UploadTarget != UploadTargetCPA {
		t.Fatal("CPA upload uncertainty lost the saved authorization or target")
	}
	if strings.Contains(state.ReloginUploadError, "cpa-private-canary") || !strings.Contains(operationCompletionPrompt(state), "CPA") {
		t.Fatal("CPA result disclosed the key or named the wrong backend")
	}
	values, _ := m.InputCopy(job.ID)
	if values["cpaConfig"] != nil || values["sub2apiConfig"] != nil || values["uploadTarget"] != nil {
		t.Fatal("completed upload retained private configuration")
	}
	data, err := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
	if err != nil || strings.Contains(string(data), "canary") || !strings.Contains(string(data), `"upload_target": "cpa"`) {
		t.Fatal("CPA metadata lost its target or persisted credentials", err)
	}
	restored := NewManager(context.Background(), 1, m.store, nil)
	restored.SetCredentialStore(vault)
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, _ = restored.Get(job.ID)
	if state.UploadTarget != UploadTargetCPA || len(restored.pending) != 0 || state.Input["cpaConfig"] != nil {
		t.Fatal("restart changed the destination or retained an executable upload")
	}
	state, err = m.QueueOperation(job.ID, "relogin", nil)
	if err != nil || state.UploadTarget != "" || state.UploadAfterRelogin {
		t.Fatal("independent operation retained a previous CPA upload", err)
	}
}

func TestCPAWorkflowRejectsBPSBeforeAdmission(t *testing.T) {
	for _, operation := range []string{"relogin", "reset-2fa"} {
		t.Run(operation, func(t *testing.T) {
			m, _, job, _ := resetManagerFixture(t)
			m.jobs[job.ID].AuthorizationMethod = AuthorizationMethodBPS
			before, _ := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
			_, err := m.QueueOperation(job.ID, operation, map[string]any{
				"uploadTarget": "cpa", "uploadAfterRelogin": true, "uploadAfterTotp": true, "reloginAfterTotp": true,
				"cpaConfig": map[string]any{"baseUrl": "https://cpa.example.test", "adminApiKey": "private-canary"},
			})
			if err == nil || !strings.Contains(err.Error(), "BPS") || len(m.pending) != 0 {
				t.Fatal("incompatible authorization was admitted", err)
			}
			after, _ := os.ReadFile(filepath.Join(m.store.Root, "job-meta.json"))
			if string(before) != string(after) {
				t.Fatal("rejected destination changed account metadata")
			}
		})
	}
}

func TestUploadDestinationDoesNotReusePriorOperation(t *testing.T) {
	for _, invalid := range []any{nil, false, "unknown", "CPA"} {
		m, _, job, _ := resetManagerFixture(t)
		if _, err := m.QueueOperation(job.ID, "relogin", map[string]any{"uploadTarget": invalid}); err == nil {
			t.Fatalf("invalid target admitted: %v", invalid)
		}
	}
	m, _, job, _ := resetManagerFixture(t)
	job, err := m.QueueOperation(job.ID, "relogin", map[string]any{
		"uploadAfterRelogin": true, "uploadTarget": "cpa", "cpaConfig": map[string]any{"adminApiKey": "cpa-canary"},
	})
	if err != nil || !m.Cancel(job.ID) {
		t.Fatal("could not cancel queued CPA workflow", err)
	}
	job = queueReloginUploadFixture(t, m, job)
	if job.UploadTarget != UploadTargetSub2API || job.Input["cpaConfig"] != nil || job.Input["uploadTarget"] != UploadTargetSub2API {
		t.Fatal("legacy upload inherited the previous CPA destination")
	}
}
