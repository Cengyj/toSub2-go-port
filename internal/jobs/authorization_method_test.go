package jobs

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

func TestAuthorizationMethodPersistsAndAutomaticRepairRetainsIt(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), 1, st, nil)
	job, err := m.Submit(Spec{Email: "bps@example.test", ImportOnly: true, Input: map[string]any{"authorizationMethod": "bps"}})
	if err != nil {
		t.Fatal(err)
	}
	m = NewManager(context.Background(), 1, st, nil)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, _ := m.Get(job.ID)
	if restored.AuthorizationMethod != "bps" {
		t.Fatal("restart lost authorization method")
	}
	queued, err := m.QueueAutoRepair(job.ID, []int{42}, "https://sub2.example.test")
	if err != nil || queued.AuthorizationMethod != "bps" {
		t.Fatal("monitor changed authorization method", err)
	}
	m.Cancel(job.ID)
	retried, err := m.RetryWithValues(job.ID, nil)
	if err != nil || retried.AuthorizationMethod != "bps" {
		t.Fatal("retry changed authorization method", err)
	}
}

func TestDiscoveredAuthorizationMethodUsesGrantIdentity(t *testing.T) {
	for _, tc := range []struct {
		name               string
		credentials, extra map[string]any
		checkpoint         map[string]any
		persisted          string
		want               string
		wantError          bool
	}{
		{name: "bps-credentials", credentials: map[string]any{"client_id": protocol.DefaultBPSClientID}, want: "bps"},
		{name: "bps-extra-compatibility", extra: map[string]any{"client_id": protocol.DefaultBPSClientID}, want: "bps"},
		{name: "chatgpt-client", credentials: map[string]any{"client_id": protocol.DefaultCodexClientID}, want: "chatgpt"},
		{name: "old-no-client", want: "chatgpt"},
		{name: "conflicting-clients", credentials: map[string]any{"client_id": protocol.DefaultBPSClientID}, extra: map[string]any{"client_id": protocol.DefaultCodexClientID}, wantError: true},
		{name: "custom-chatgpt-client", credentials: map[string]any{"client_id": "custom-cli-client"}, want: "chatgpt"},
		{name: "custom-chatgpt-client-extra", extra: map[string]any{"client_id": "custom-cli-client"}, want: "chatgpt"},
		{name: "conflicting-custom-clients", credentials: map[string]any{"client_id": "custom-cli-client"}, extra: map[string]any{"client_id": "other-custom-client"}, wantError: true},
		{name: "custom-chatgpt-checkpoint", checkpoint: map[string]any{"oauth": map[string]any{"clientId": "custom-cli-client"}}, want: "chatgpt"},
		{name: "custom-client-cannot-claim-bps", checkpoint: map[string]any{"authorization_method": "bps", "oauth": map[string]any{"clientId": "custom-cli-client"}}, wantError: true},
		{name: "bps-checkpoint", checkpoint: map[string]any{"authorization_method": "bps", "oauth": map[string]any{"clientId": protocol.DefaultBPSClientID}}, want: "bps"},
		{name: "bps-checkpoint-client-only", checkpoint: map[string]any{"oauth": map[string]any{"clientId": protocol.DefaultBPSClientID}}, want: "bps"},
		{name: "conflicting-checkpoint", checkpoint: map[string]any{"authorization_method": "chatgpt", "oauth": map[string]any{"clientId": protocol.DefaultBPSClientID}}, wantError: true},
		{name: "explicit-selection-with-old-export", credentials: map[string]any{"client_id": protocol.DefaultCodexClientID}, persisted: "bps", want: "bps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, _ := store.New(t.TempDir())
			artifacts, _ := store.New(filepath.Join(st.Root, "imported"))
			write := func(name string, value any) {
				t.Helper()
				if err := artifacts.WriteJSON(ctx, name, value); err != nil {
					t.Fatal(err)
				}
			}
			if tc.persisted != "" {
				write("job-meta.json", map[string]any{"email": "imported@example.test", "authorization_method": tc.persisted})
			}
			artifactName := "sub2api-import-oauth.json"
			if tc.checkpoint != nil {
				artifactName = "login-checkpoint.json"
				tc.checkpoint["version"], tc.checkpoint["email"], tc.checkpoint["stage"] = 1, "imported@example.test", "bps_continue"
				write("login-checkpoint.json", tc.checkpoint)
			} else {
				if tc.credentials == nil {
					tc.credentials = map[string]any{}
				}
				tc.credentials["email"] = "imported@example.test"
				write("sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": tc.credentials, "extra": tc.extra}}})
			}
			originalArtifact, err := readRegularJSON(filepath.Join(st.Root, "imported", artifactName))
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(ctx, 1, st, nil)
			err = m.SyncOutputs(ctx, true)
			if (err != nil) != tc.wantError {
				t.Fatalf("discovery error=%v, wantError=%t", err, tc.wantError)
			}
			if tc.wantError {
				if _, exists := m.Get("imported"); exists {
					t.Fatal("conflicting grant was imported as a default provider")
				}
				return
			}
			job, exists := m.Get("imported")
			if !exists || job.AuthorizationMethod != tc.want {
				t.Fatalf("discovered method: %+v", job)
			}
			if tc.checkpoint != nil && job.Status != "resume_available" {
				t.Fatal("checkpoint no longer resumable")
			}
			restored := NewManager(ctx, 1, st, nil)
			if err := restored.Restore(ctx); err != nil {
				t.Fatal(err)
			}
			job, _ = restored.Get("imported")
			if job.AuthorizationMethod != tc.want {
				t.Fatal("persisted discovery lost provider on restart")
			}
			afterRestore, err := readRegularJSON(filepath.Join(st.Root, "imported", artifactName))
			if err != nil || !reflect.DeepEqual(originalArtifact, afterRestore) {
				t.Fatal("discovery or restore changed the grant's original OAuth identity")
			}
		})
	}
}

func TestAuthorizationMethodSwitchStartsNewLoginAndProtectsResume(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	job, err := m.Submit(Spec{Email: "switch@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if job.AuthorizationMethod != "chatgpt" {
		t.Fatal("default no longer uses ChatGPT")
	}
	queued, err := m.QueueOperation(job.ID, "regenerate", map[string]any{"authorizationMethod": "bps"})
	if err != nil || queued.Operation != "relogin" || queued.AuthorizationMethod != "bps" {
		t.Fatal("provider switch reused old refresh grant", err)
	}
	m.Cancel(job.ID)
	m.Emit(JobEvent{JobID: job.ID, Status: "resume_available"})
	before, _ := m.Get(job.ID)
	if _, err := m.RetryWithValues(job.ID, map[string]any{"authorizationMethod": "chatgpt", "password": "must-not-apply"}); err == nil {
		t.Fatal("checkpoint reused across providers")
	}
	after, _ := m.Get(job.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected provider switch mutated job")
	}
	continued, err := m.RetryWithValues(job.ID, nil)
	if err != nil || continued.Operation != "resume" || continued.AuthorizationMethod != "bps" {
		t.Fatal("same-provider resume was broken", err)
	}
}

func TestInvalidAuthorizationMethodCannotEnterManager(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	bad := map[string]any{"authorizationMethod": "other"}
	if _, err := m.Submit(Spec{Email: "invalid@example.test", Input: bad}); err == nil || len(m.List()) != 0 {
		t.Fatal("invalid submission queued")
	}
	job, _ := m.Submit(Spec{Email: "owner@example.test", ImportOnly: true})
	before, _ := m.Get(job.ID)
	for _, apply := range []func() error{
		func() error { _, err := m.Update(job.ID, bad); return err },
		func() error { _, err := m.RetryWithValues(job.ID, bad); return err },
		func() error { _, err := m.QueueOperation(job.ID, "relogin", bad); return err },
		func() error {
			_, _, err := m.SubmitUniqueEmail(Spec{Email: job.Email, ImportOnly: true, Input: bad})
			return err
		},
	} {
		if apply() == nil {
			t.Fatal("invalid authorization method accepted")
		}
		after, _ := m.Get(job.ID)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("invalid authorization method modified job")
		}
	}
}

func TestRetryDifferentAuthorizationMethodDoesNotReuseOldCheckpoint(t *testing.T) {
	for _, status := range []string{StatusFailed, StatusCanceled} {
		for _, security := range []bool{false, true} {
			t.Run(string(status)+map[bool]string{false: "", true: "-security-check"}[security], func(t *testing.T) {
				ctx := context.Background()
				st, _ := store.New(t.TempDir())
				m := NewManager(ctx, 1, st, nil)
				job, err := m.Submit(Spec{ID: "retry", Email: "retry@example.test", ImportOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				m.Emit(JobEvent{JobID: job.ID, Status: status})
				m.mu.Lock()
				m.jobs[job.ID].SecurityCheckRequired = security
				m.mu.Unlock()
				artifacts, _ := store.New(filepath.Join(st.Root, job.ID))
				if err := artifacts.WriteJSON(ctx, "login-checkpoint.json", map[string]any{"version": 1, "email": job.Email, "authorization_method": "chatgpt", "stage": "oauth_continue"}); err != nil {
					t.Fatal(err)
				}
				queued, err := m.RetryWithValues(job.ID, map[string]any{"authorizationMethod": "bps"})
				if err != nil || queued.Operation != "relogin" || queued.AuthorizationMethod != "bps" {
					t.Fatalf("provider switch reused old checkpoint: job=%+v err=%v", queued, err)
				}
				if queued.LastOperationType != "initial_authorization" {
					t.Fatal("first authorization lost its operation label")
				}
			})
		}
	}
}
