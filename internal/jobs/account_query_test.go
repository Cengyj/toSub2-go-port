package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func accountQueryManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 2, st, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return m, st
}

func accountQueryJob(t *testing.T, m *Manager, id, email string) *Job {
	t.Helper()
	j, err := m.Submit(Spec{ID: id, Email: email, ImportOnly: true, Input: map[string]any{"password": "synthetic-private-password", "proxyUrl": ""}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func accountQueryLease(t *testing.T, m *Manager, ctx context.Context, id string) *AccountQueryLease {
	t.Helper()
	lease, err := m.ReserveAccountQuery(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	return lease
}

func TestAccountQueryPreservesHistoryAndCoordinatesNormalizedEmail(t *testing.T) {
	m, st := accountQueryManager(t)
	job := accountQueryJob(t, m, "query-first", "Owner@Example.test")
	peer := accountQueryJob(t, m, "query-peer", " owner@example.test ")
	other := accountQueryJob(t, m, "query-other", "different@example.test")
	before, _ := m.Get(job.ID)
	beforeDisk, err := os.ReadFile(st.Path("job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	if !lease.Valid() || lease.Job().Input != nil || lease.job.Input != nil {
		t.Fatal("lease is invalid or exposes authentication input")
	}
	if _, err := m.ReserveAccountQuery(t.Context(), peer.ID); !errors.Is(err, ErrAccountQueryBusy) {
		t.Fatal("same email admitted overlapping queries", err)
	}
	otherLease := accountQueryLease(t, m, t.Context(), other.ID)
	otherLease.Release()
	public := lease.Job()
	public.Email = "mutated@example.test"
	lease.Release()
	lease.Release()
	after, _ := m.Get(job.ID)
	afterDisk, err := os.ReadFile(st.Path("job-meta.json"))
	if err != nil || !reflect.DeepEqual(before, after) || !bytes.Equal(beforeDisk, afterDisk) {
		t.Fatal("read-only reservation changed job metadata or history", err)
	}
	accountQueryLease(t, m, t.Context(), peer.ID).Release()
}

func TestAccountQueryYieldsToAccountOperationsBeforeRunnerStarts(t *testing.T) {
	for _, operation := range []string{"login", "relogin", "refresh", "reset-2fa"} {
		t.Run(operation, func(t *testing.T) {
			m, _ := accountQueryManager(t)
			job := accountQueryJob(t, m, "query-priority", "owner@example.test")
			lease := accountQueryLease(t, m, t.Context(), job.ID)
			var released atomic.Bool
			started := make(chan bool, 1)
			m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { started <- released.Load(); return nil })
			m.Start()
			if _, err := m.QueueOperation(job.ID, operation, nil); err != nil {
				t.Fatal(err)
			}
			if lease.Context().Err() == nil || lease.Valid() {
				t.Fatal("new operation did not preempt the query")
			}
			m.mu.Lock()
			ready := m.nextRunnableLocked()
			m.mu.Unlock()
			if ready != -1 {
				t.Fatal("runner could start before canceled query released its transport")
			}
			if _, err := m.ReserveAccountQuery(t.Context(), job.ID); !errors.Is(err, ErrAccountQueryBusy) {
				t.Fatal("query bypassed queued operation", err)
			}
			released.Store(true)
			lease.Release()
			select {
			case safe := <-started:
				if !safe {
					t.Fatal("authentication overlapped the old query")
				}
			case <-time.After(time.Second):
				t.Fatal("query release did not wake the account operation")
			}
		})
	}
}

func TestAccountQueryInputAndImportChangesInvalidateSnapshot(t *testing.T) {
	m, st := accountQueryManager(t)
	job := accountQueryJob(t, m, "query-update", "owner@example.test")
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	if _, err := m.Update(job.ID, map[string]any{"proxyUrl": "http://proxy.example.test:8080"}); err != nil {
		t.Fatal(err)
	}
	if lease.Context().Err() == nil || lease.Valid() {
		t.Fatal("proxy update did not invalidate the query")
	}
	lease.Release()
	lease = accountQueryLease(t, m, t.Context(), job.ID)
	discoveryFixture(t, st.Root, "new-import", job.Email, "", true, false)
	if err := m.SyncOutputs(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if lease.Context().Err() == nil || lease.Valid() {
		t.Fatal("discovery did not preempt a query for the same email")
	}
	if _, found := m.Get("new-import"); found {
		t.Fatal("discovery overlapped a live query reservation")
	}
	lease.Release()
	if err := m.SyncOutputs(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, found := m.Get("new-import"); !found {
		t.Fatal("discovery remained blocked after query cleanup")
	}
}

func TestAccountQueryDeletionWaitsForReleaseAndCannotResurrectAccount(t *testing.T) {
	m, st := accountQueryManager(t)
	job := accountQueryJob(t, m, "query-delete", "owner@example.test")
	discoveryFixture(t, st.Root, job.ID, job.Email, "", true, false)
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	deleted := make(chan error, 1)
	go func() { _, err := m.DeleteAccounts(t.Context(), []string{job.ID}); deleted <- err }()
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("delete did not cancel query")
	}
	if lease.Valid() {
		t.Fatal("deleting account accepted a stale query result")
	}
	if _, err := os.Stat(filepath.Join(st.Root, job.ID)); err != nil {
		t.Fatal("delete removed private files before query cleanup", err)
	}
	select {
	case err := <-deleted:
		t.Fatal("delete completed before query release", err)
	default:
	}
	lease.Release()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete did not complete")
	}
	if _, exists := m.Get(job.ID); exists || lease.Valid() {
		t.Fatal("deleted account was resurrected")
	}
	if _, err := m.ReserveAccountQuery(t.Context(), job.ID); err == nil {
		t.Fatal("query admitted deleted account")
	}
}

func TestAccountQueryCancellationAndShutdownWaitForOwnedCleanup(t *testing.T) {
	m, _ := accountQueryManager(t)
	job := accountQueryJob(t, m, "query-cancel", "owner@example.test")
	ctx, cancel := context.WithCancel(t.Context())
	lease := accountQueryLease(t, m, ctx, job.ID)
	cancel()
	if lease.Valid() {
		t.Fatal("canceled query remained valid")
	}
	if _, err := m.ReserveAccountQuery(t.Context(), job.ID); !errors.Is(err, ErrAccountQueryBusy) {
		t.Fatal("cancellation prematurely released the transport owner", err)
	}
	lease.Release()
	lease = accountQueryLease(t, m, t.Context(), job.ID)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer closeCancel()
	if err := m.Shutdown(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("shutdown failed to wait for query cleanup", err)
	}
	if lease.Context().Err() == nil || lease.Valid() {
		t.Fatal("shutdown left query active")
	}
	lease.Release()
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAccountQueryRejectsRecoveryAndNeverLoadsVaultUnderSchedulerLock(t *testing.T) {
	m, _ := accountQueryManager(t)
	job := accountQueryJob(t, m, "query-recovery", "owner@example.test")
	m.SetCredentialStore(&queryForbiddenVault{t: t})
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	lease.Release()
	m.mu.Lock()
	m.jobs[job.ID].TOTPRecoveryPending = true
	m.mu.Unlock()
	if _, err := m.ReserveAccountQuery(t.Context(), job.ID); !errors.Is(err, ErrTOTPRecoveryPending) {
		t.Fatal("query ignored 2FA recovery isolation", err)
	}
}

func TestAccountQueryCredentialUpdatesInvalidateIdentityAndRevision(t *testing.T) {
	m, _ := accountQueryManager(t)
	job, err := m.Submit(Spec{ID: "query-proxy-revision", Email: "owner@example.test", ImportOnly: true, Input: map[string]any{"proxyUrl": "http://first.example.test:8080"}})
	if err != nil {
		t.Fatal(err)
	}
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	m.UpdateStoredCredentials(job.ID, credentials.Credentials{ProxyURL: "http://second.example.test:8080"})
	updated, _ := m.Get(job.ID)
	if lease.Valid() || lease.Context().Err() == nil || !updated.UpdatedAt.After(job.UpdatedAt) || !updated.ProxyConfigured {
		t.Fatal("same-mode proxy replacement kept the query or its source revision current")
	}
	lease.Release()
	lease = accountQueryLease(t, m, t.Context(), job.ID)
	m.SetCredentialStore(credentials.NewMemoryStore())
	if lease.Valid() || lease.Context().Err() == nil {
		t.Fatal("credential backend replacement kept the old query valid")
	}
}

func TestAccountQueryValidChecksExactProxyInput(t *testing.T) {
	for _, changed := range []any{"http://replacement.example.test:8080", nil, 123} {
		m, _ := accountQueryManager(t)
		job := accountQueryJob(t, m, "query-proxy-input", "owner@example.test")
		lease := accountQueryLease(t, m, t.Context(), job.ID)
		m.mu.Lock()
		if changed == nil {
			delete(m.jobs[job.ID].Input, "proxyUrl")
		} else {
			m.jobs[job.ID].Input["proxyUrl"] = changed
		}
		m.mu.Unlock()
		if lease.Valid() {
			t.Fatal("proxy identity changed without invalidating the lease")
		}
		lease.Release()
	}
}

type queryForbiddenVault struct{ t *testing.T }

func (v *queryForbiddenVault) Load(string) (credentials.Credentials, error) {
	v.t.Error("query admission unexpectedly loaded credentials")
	return credentials.Credentials{}, credentials.ErrUnavailable
}
func (v *queryForbiddenVault) Save(string, credentials.Credentials) error {
	return credentials.ErrUnavailable
}
func (v *queryForbiddenVault) Delete(string) error { return credentials.ErrUnavailable }
