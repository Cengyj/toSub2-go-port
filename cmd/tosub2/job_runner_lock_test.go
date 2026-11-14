package main

import (
	"errors"
	"strings"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestRunnerLocalCompletionAccountLockAndRelease(t *testing.T) {
	for _, operation := range []string{"complete-oauth-grant", "complete-2fa"} {
		t.Run(operation, func(t *testing.T) {
			cfg := sessionAuditConfig(t.TempDir(), "")
			vault := credentials.NewMemoryStore()
			manager, _, logger := sessionAuditManager(t, cfg, vault)
			job, err := manager.Submit(jobs.Spec{ID: "lock-check", Email: "runner-lock-" + operation + "@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			job.Operation = operation
			runner := makeJobRunnerWithCredentials(cfg, manager, logger, vault, true)
			release, err := accountlock.Acquire("https://chatgpt.com", job.Email)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = release() })
			if err := runner(t.Context(), job, func(jobs.JobEvent) {}); !errors.Is(err, accountlock.ErrBusy) {
				t.Fatalf("local completion did not respect the default account origin: %v", err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			if err := runner(t.Context(), job, func(jobs.JobEvent) {}); err == nil || errors.Is(err, accountlock.ErrBusy) {
				t.Fatalf("missing local evidence should fail after acquiring the account lock: %v", err)
			}
			released, err := accountlock.Acquire("https://chatgpt.com", job.Email)
			if err != nil {
				t.Fatalf("early completion failure leaked the account lock: %v", err)
			}
			if err := released(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerRemovedOperationRejectsBeforeAccountLock(t *testing.T) {
	cfg := sessionAuditConfig(t.TempDir(), "")
	vault := credentials.NewMemoryStore()
	manager, _, logger := sessionAuditManager(t, cfg, vault)
	job, err := manager.Submit(jobs.Spec{ID: "removed-lock-check", Email: "removed-lock@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	job.Operation = "recover-2fa"
	release, err := accountlock.Acquire("https://chatgpt.com", job.Email)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = makeJobRunnerWithCredentials(cfg, manager, logger, vault, true)(t.Context(), job, func(jobs.JobEvent) {})
	if err == nil || !strings.Contains(err.Error(), "TOTP_OPERATION_REMOVED") {
		t.Fatalf("removed operation crossed its precondition boundary: %v", err)
	}
}
