package application

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestAccountInsightGrantRejectsSharedVaultRecoveryWithoutLocalJournal(t *testing.T) {
	for _, proxy := range []struct {
		name  string
		input any
	}{
		{name: "global-fallback"},
		{name: "explicit-direct", input: ""},
	} {
		for _, pending := range []struct {
			name string
			mark func(*credentials.Credentials)
			want error
		}{
			{
				name: "reset-unknown",
				mark: func(saved *credentials.Credentials) {
					saved.TOTPResetPending = true
					saved.TOTPResetOperationID = "external-synthetic-reset"
				},
				want: jobs.ErrTOTPRecoveryPending,
			},
			{
				name: "setup-unverified",
				mark: func(saved *credentials.Credentials) {
					saved.PendingTOTPSecret = "JBSWY3DPEHPK3PXP"
					saved.PendingTOTPFactorID = "external-synthetic-factor"
				},
				want: jobs.ErrTOTPSetupVerificationPending,
			},
		} {
			t.Run(proxy.name+"/"+pending.name, func(t *testing.T) {
				f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, proxy.input)
				initial, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID)
				if err != nil {
					t.Fatal("synthetic committed grant was not initially available", err)
				}
				initial.Release()

				// Another manager can share the credential store while its own
				// operation history is absent from this manager's job-meta file.
				// Model only its durable quarantine marker: no login or remote
				// mutation runs, and no local journal can mask a missing vault gate.
				saved, err := f.vault.Load(f.job.Email)
				if err != nil {
					t.Fatal(err)
				}
				pending.mark(&saved)
				if err := f.vault.Save(f.job.Email, saved); err != nil {
					t.Fatal(err)
				}
				if _, err := f.manager.LoadCredentialsContext(t.Context(), f.job.ID); !errors.Is(err, pending.want) {
					t.Fatal("control credential read did not enforce the shared quarantine", err)
				}
				beforeJobs := f.manager.List()
				beforeMeta, err := os.ReadFile(f.store.Path("job-meta.json"))
				if err != nil {
					t.Fatal(err)
				}
				beforeExport, err := os.ReadFile(f.path)
				if err != nil {
					t.Fatal(err)
				}

				grant, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID)
				if grant != nil {
					grant.Release()
				}
				if grant != nil || !errors.Is(err, pending.want) {
					t.Error("query admitted a committed grant despite shared-vault 2FA quarantine", err)
				}
				if f.manager.ActivitySnapshot().Queries != 0 {
					t.Error("denied query retained account ownership")
				}
				afterMeta, metaErr := os.ReadFile(f.store.Path("job-meta.json"))
				afterExport, exportErr := os.ReadFile(f.path)
				afterSaved, vaultErr := f.vault.Load(f.job.Email)
				if metaErr != nil || exportErr != nil || vaultErr != nil ||
					!bytes.Equal(beforeMeta, afterMeta) || !bytes.Equal(beforeExport, afterExport) ||
					!reflect.DeepEqual(beforeJobs, f.manager.List()) || !credentials.EqualReadback(saved, afterSaved) {
					t.Error("query changed account history, committed authorization or shared recovery evidence", metaErr, exportErr, vaultErr)
				}
			})
		}
	}
}

func TestAccountInsightGrantSharedVaultReadOutcomes(t *testing.T) {
	for _, scenario := range []string{"nil-store", "unreadable-store", "missing-record"} {
		t.Run(scenario, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
			switch scenario {
			case "nil-store":
				f.manager.SetCredentialStore(nil)
			case "unreadable-store":
				vault := &insightQueryFaultVault{base: f.vault}
				vault.fail(&os.PathError{Op: "decrypt", Path: "synthetic-private-vault-location", Err: os.ErrPermission})
				f.manager.SetCredentialStore(vault)
			case "missing-record":
				if err := f.vault.Delete(f.job.Email); err != nil {
					t.Fatal(err)
				}
			}
			grant, err := LoadAccountInsightGrant(t.Context(), f.store.Root, f.manager, f.job.ID)
			if grant != nil {
				defer grant.Release()
			}
			if scenario == "missing-record" {
				if err != nil || grant == nil {
					t.Fatal("definitely absent credentials prevented using a committed grant", err)
				}
				if err := grant.Validate(); err != nil {
					t.Fatal("definitely absent credentials invalidated the committed grant", err)
				}
				grant.Release()
			} else if grant != nil || !errors.Is(err, ErrInsightCredentialsUnavailable) {
				t.Fatal("unreadable security state was treated as absent credentials", err)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-private") {
				t.Error("credential failure exposed the private storage diagnostic")
			}
			if f.manager.ActivitySnapshot().Queries != 0 {
				t.Error("credential outcome retained query ownership")
			}
		})
	}
}

func TestAccountInsightGrantSharedVaultValidation(t *testing.T) {
	for _, scenario := range []string{"reset-unknown", "setup-unverified", "unreadable-store", "missing-record"} {
		t.Run(scenario, func(t *testing.T) {
			f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, "")
			vault := &insightQueryFaultVault{base: f.vault}
			f.manager.SetCredentialStore(vault)
			grant := f.load(t)
			saved, err := f.vault.Load(f.job.Email)
			if err != nil {
				t.Fatal(err)
			}
			// Keep the manager and store object unchanged so only a fresh vault
			// read, rather than a local version change, can detect the new state.
			switch scenario {
			case "reset-unknown":
				saved.TOTPResetPending = true
				saved.TOTPResetOperationID = "late-external-synthetic-reset"
				if err := f.vault.Save(f.job.Email, saved); err != nil {
					t.Fatal(err)
				}
			case "setup-unverified":
				saved.PendingTOTPSecret = "JBSWY3DPEHPK3PXP"
				saved.PendingTOTPFactorID = "late-external-synthetic-factor"
				if err := f.vault.Save(f.job.Email, saved); err != nil {
					t.Fatal(err)
				}
			case "unreadable-store":
				vault.fail(errors.New("synthetic-private-vault-decryption-failure"))
			case "missing-record":
				if err := f.vault.Delete(f.job.Email); err != nil {
					t.Fatal(err)
				}
			}
			if err := grant.Validate(); scenario == "missing-record" {
				if err != nil {
					t.Fatal("definitely absent credential record invalidated a committed grant", err)
				}
			} else if !errors.Is(err, jobs.ErrAccountQuerySuperseded) {
				t.Fatal("query accepted a result after its shared security state became unsafe", err)
			}
			// Validation does not release a grant while its caller may still be
			// finishing network work; only the explicit owner releases the lock.
			if f.manager.ActivitySnapshot().Queries != 1 {
				t.Error("validation released query ownership before the caller finished")
			}
			grant.Release()
			if f.manager.ActivitySnapshot().Queries != 0 {
				t.Error("explicit release retained the invalidated query")
			}
		})
	}
}

func TestAccountInsightGrantSharedVaultCancellationReleasesOwnership(t *testing.T) {
	f := newInsightGrantFixture(t, jobs.AuthorizationMethodChatGPT, nil)
	vault := &insightBlockingVault{entered: make(chan struct{})}
	f.manager.SetCredentialStore(vault)
	const origin = "https://account-query-shared-vault-cancel.synthetic.invalid"
	var acquisitions, releases atomic.Int32
	f.manager.SetAccountOperationLease(func(email string) (func() error, error) {
		release, err := accountlock.Acquire(origin, email)
		if err != nil {
			return nil, err
		}
		acquisitions.Add(1)
		return func() error {
			err := release()
			releases.Add(1)
			return err
		}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		grant, err := LoadAccountInsightGrant(ctx, f.store.Root, f.manager, f.job.ID)
		if grant != nil {
			grant.Release()
		}
		done <- err
	}()
	select {
	case <-vault.entered:
	case <-time.After(time.Second):
		t.Fatal("direct query did not check the shared security state")
	}
	if peerRelease, err := accountlock.Acquire(origin, f.job.Email); !errors.Is(err, accountlock.ErrBusy) {
		if peerRelease != nil {
			_ = peerRelease()
		}
		t.Fatal("shared security lookup ran without host-wide account ownership", err)
	}
	if acquisitions.Load() != 1 || releases.Load() != 0 || f.manager.ActivitySnapshot().Queries != 1 {
		t.Error("protected lookup did not retain both ownership layers")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("security lookup lost caller cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("security lookup did not respond to cancellation")
	}
	if acquisitions.Load() != 1 || releases.Load() != 1 || f.manager.ActivitySnapshot().Queries != 0 {
		t.Error("canceled security lookup repeated or leaked query ownership")
	}
	peerRelease, err := accountlock.Acquire(origin, f.job.Email)
	if err != nil {
		t.Fatal("canceled security lookup leaked host-wide ownership", err)
	}
	if err := peerRelease(); err != nil {
		t.Fatal(err)
	}
}

// A fault can appear after admission without replacing the store object. This
// models a later permission or decryption failure, not a manager generation bump.
type insightQueryFaultVault struct {
	base credentials.Store
	mu   sync.RWMutex
	err  error
}

func (v *insightQueryFaultVault) fail(err error) {
	v.mu.Lock()
	v.err = err
	v.mu.Unlock()
}

func (v *insightQueryFaultVault) Load(email string) (credentials.Credentials, error) {
	v.mu.RLock()
	err := v.err
	v.mu.RUnlock()
	if err != nil {
		return credentials.Credentials{}, err
	}
	return v.base.Load(email)
}

func (v *insightQueryFaultVault) Save(email string, saved credentials.Credentials) error {
	return v.base.Save(email, saved)
}

func (v *insightQueryFaultVault) Delete(email string) error {
	return v.base.Delete(email)
}
