package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
)

func TestTrashPurgeRespectsAccountLeaseBeforeIrreversibleCommit(t *testing.T) {
	m, st, vault := trashManager(t)
	const origin, email, id = "https://purge-lock.example.test", "purge-lock@example.test", "purge-lock"
	if _, err := m.Submit(Spec{ID: id, Email: email, ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	discoveryFixture(t, st.Root, id, email, StatusSucceeded, true, true)
	if err := vault.Save(email, credentials.Credentials{Password: "synthetic-retained-password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.TrashAccounts(t.Context(), []string{id}); err != nil {
		t.Fatal(err)
	}
	entry := m.ListTrash()[0]
	release, err := accountlock.Acquire(origin, email)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	acquisitions, hooks := 0, 0
	m.SetAccountOperationLease(func(got string) (func() error, error) {
		acquisitions++
		return accountlock.Acquire(origin, got)
	})
	m.SetTrashPurgeHook(func(context.Context, *TrashEntry) error { hooks++; return nil })
	count, err := m.PurgeTrash(t.Context(), []string{entry.ID})
	if !errors.Is(err, ErrTrashBusy) || count != 0 || acquisitions != 1 || hooks != 0 {
		t.Fatalf("purge bypassed active account lease: count=%d acquisitions=%d hooks=%d err=%v", count, acquisitions, hooks, err)
	}
	retained, ok := m.GetTrash(entry.ID)
	if !ok || retained.State != TrashStateTrashed || retained.PurgeStartedAt != nil {
		t.Fatal("busy account was irreversibly marked for deletion")
	}
	if _, err := os.Stat(filepath.Join(st.Root, id)); err != nil {
		t.Fatal("busy account lost private artifacts", err)
	}
	if value, err := vault.Load(email); err != nil || value.Password != "synthetic-retained-password" {
		t.Fatal("busy account lost credentials", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if count, err := m.PurgeTrash(t.Context(), []string{entry.ID}); err != nil || count != 1 || hooks != 1 {
		t.Fatal("released account lease did not allow purge", err)
	}
	if _, err := vault.Load(email); !errors.Is(err, os.ErrNotExist) || len(m.ListTrash()) != 0 {
		t.Fatal("accepted purge did not finish credential and tombstone cleanup")
	}
}

func TestTrashPurgeReleasesBatchLeasesBeforeCommitFailure(t *testing.T) {
	for _, failure := range []string{"busy-second", "missing-second", "cancel-first"} {
		t.Run(failure, func(t *testing.T) {
			m, _, vault := trashManager(t)
			const origin = "https://purge-batch-lock.example.test"
			ids := []string{"batch-first", "batch-second"}
			emails := []string{"batch-first@example.test", "batch-second@example.test"}
			for i, id := range ids {
				if _, err := m.Submit(Spec{ID: id, Email: emails[i], ImportOnly: true}); err != nil {
					t.Fatal(err)
				}
				if err := vault.Save(emails[i], credentials.Credentials{Password: "synthetic-retained-password"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.TrashAccounts(t.Context(), ids); err != nil {
				t.Fatal(err)
			}
			selected := m.ListTrash()
			trashIDs := []string{selected[0].ID, selected[1].ID}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "busy-second" {
				release, err := accountlock.Acquire(origin, selected[1].Email)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			m.SetAccountOperationLease(func(email string) (func() error, error) {
				if failure == "missing-second" && email == selected[1].Email {
					return nil, nil
				}
				release, err := accountlock.Acquire(origin, email)
				if failure == "cancel-first" {
					cancel()
				}
				return release, err
			})
			hooks := 0
			m.SetTrashPurgeHook(func(context.Context, *TrashEntry) error { hooks++; return nil })
			count, err := m.PurgeTrash(ctx, trashIDs)
			want := map[string]error{"busy-second": ErrTrashBusy, "missing-second": ErrTrashLocalCleanup, "cancel-first": context.Canceled}[failure]
			if count != 0 || !errors.Is(err, want) || hooks != 0 {
				t.Fatalf("failed lease acquisition changed the batch: count=%d hooks=%d err=%v", count, hooks, err)
			}
			for _, entry := range m.ListTrash() {
				if entry.State != TrashStateTrashed || entry.PurgeStartedAt != nil {
					t.Fatal("incomplete lease batch published an irreversible marker")
				}
				if value, err := vault.Load(entry.Email); err != nil || value.Password == "" {
					t.Fatal("incomplete lease batch removed credentials", err)
				}
			}
			release, err := accountlock.Acquire(origin, selected[0].Email)
			if err != nil {
				t.Fatal("failed batch leaked an earlier account lease", err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type trashLeaseDeleteHook struct {
	credentials.Store
	deleting func(string)
}

func (v *trashLeaseDeleteHook) Delete(email string) error {
	v.deleting(email)
	return v.Store.Delete(email)
}

func TestTrashPurgeHoldsLeaseThroughCleanupAndReleasesOnExit(t *testing.T) {
	for _, outcome := range []string{"success", "hook-failure", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			m, _, vault := trashManager(t)
			const origin, email = "https://purge-lifetime.example.test", "purge-lifetime@example.test"
			if _, err := m.Submit(Spec{ID: "lifetime", Email: email, ImportOnly: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.TrashAccounts(t.Context(), []string{"lifetime"}); err != nil {
				t.Fatal(err)
			}
			assertHeld := func(got string) {
				release, err := accountlock.Acquire(origin, got)
				if release != nil {
					_ = release()
				}
				if got != email || !errors.Is(err, accountlock.ErrBusy) {
					t.Fatal("purge released its ordinary account lease before cleanup finished")
				}
			}
			deletes := 0
			m.SetCredentialStore(&trashLeaseDeleteHook{Store: vault, deleting: func(email string) { deletes++; assertHeld(email) }})
			m.SetAccountOperationLease(func(email string) (func() error, error) { return accountlock.Acquire(origin, email) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m.SetTrashPurgeHook(func(_ context.Context, entry *TrashEntry) error {
				assertHeld(entry.Email)
				if outcome == "hook-failure" {
					return errors.New("synthetic cleanup failure")
				}
				if outcome == "canceled" {
					cancel()
				}
				return nil
			})
			count, err := m.PurgeTrash(ctx, []string{m.ListTrash()[0].ID})
			if outcome == "success" {
				if count != 1 || err != nil || deletes != 1 || len(m.ListTrash()) != 0 {
					t.Fatal("purge did not complete after keeping its lease", err)
				}
			} else if count != 0 || err == nil || deletes != 0 || len(m.ListTrash()) != 1 || m.ListTrash()[0].State != TrashStatePurging {
				t.Fatal("interrupted cleanup lost its retryable tombstone", err)
			}
			release, err := accountlock.Acquire(origin, email)
			if err != nil {
				t.Fatal("purge leaked its lease on exit", err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
