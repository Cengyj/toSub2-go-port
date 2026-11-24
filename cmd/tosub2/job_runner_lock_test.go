package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestConfiguredQueryLeaseMatchesOperationOrigin(t *testing.T) {
	for _, origin := range []string{"", "https://STRICT-QUERY.example.test:443/settings"} {
		t.Run(origin, func(t *testing.T) {
			m := jobs.NewManager(t.Context(), 1, nil, nil)
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			configureAccountOperationLease(config.Config{ChatGPTBase: origin}, m)
			job, err := m.Submit(jobs.Spec{ID: "configured-query", Email: "configured-query@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			query, err := m.ReserveAccountQuery(t.Context(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer query.Release()
			peerOrigin := "https://strict-query.example.test/other"
			if origin == "" {
				peerOrigin = "https://CHATGPT.COM:443/other"
			}
			release, err := accountlock.Acquire(peerOrigin, "  "+strings.ToUpper(job.Email)+"  ")
			if release != nil {
				_ = release()
			}
			if !errors.Is(err, accountlock.ErrBusy) {
				t.Fatal("query and runner lock origins differ", err)
			}
			query.Release()
			release, err = accountlock.Acquire(peerOrigin, job.Email)
			if err != nil {
				t.Fatal("query cleanup retained runner lock", err)
			}
			_ = release()
		})
	}
}

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
