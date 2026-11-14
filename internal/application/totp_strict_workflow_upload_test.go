package application

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

// Exercise the real automatic upload entry after the authentication operation
// has returned and released its OS lease, while scheduler ownership remains.
func TestStrictWorkflowUploadOwnsCrossInstanceAccountLock(t *testing.T) {
	for _, scenario := range []string{"other-instance-first", "upload-first"} {
		t.Run(scenario, func(t *testing.T) {
			var reads, writes atomic.Int32
			mutationStarted, releaseMutation := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			finishMutation := func() { releaseOnce.Do(func() { close(releaseMutation) }) }
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					reads.Add(1)
					_, _ = w.Write([]byte(`{"data":{"items":[],"pages":1,"total":0}}`))
					return
				}
				writes.Add(1)
				if scenario == "upload-first" {
					close(mutationStarted)
					select {
					case <-releaseMutation:
					case <-r.Context().Done():
						return
					}
				}
				_, _ = w.Write([]byte(`{"created":1}`))
			}))
			t.Cleanup(remote.Close)
			t.Cleanup(finishMutation)
			disk, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := jobs.NewManager(t.Context(), 1, disk, nil)
			manager.SetCredentialStore(credentials.NewMemoryStore())
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			job, err := manager.Submit(jobs.Spec{ID: "strict-workflow-upload", Email: "workflow-lock@example.test", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			writePrivateGrantFixture(t, filepath.Join(disk.Root, job.ID, "sub2api-import-oauth.json"), `{"type":"sub2api-data","accounts":[{"platform":"openai","type":"oauth","credentials":{"email":"workflow-lock@example.test","access_token":"synthetic-workflow-access"}}]}`)
			controller, err := NewAccountQualityController(config.Config{OutputRoot: disk.Root, ChatGPTBase: remote.URL}, manager)
			if err != nil {
				t.Fatal(err)
			}
			manager.SetAuthorizationSyncCheck(controller.WorkflowUploadCheck)
			finished := make(chan error, 1)
			operation := func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				release, err := accountlock.Acquire(remote.URL, active.Email)
				if err != nil {
					return err
				}
				defer release()
				manager.UpdateResult(active.ID, true, "synthetic committed authorization")
				return nil
			}
			upload := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				if !manager.ActiveAccountOperation(active.ID) {
					err := errors.New("automatic upload lost scheduler ownership")
					finished <- err
					return err
				}
				if scenario == "other-instance-first" {
					release, err := accountlock.Acquire(remote.URL, active.Email)
					if err != nil {
						finished <- err
						return err
					}
					defer release()
				}
				err := UploadSavedAuthorizationAtOrigin(ctx, disk.Root, remote.URL, manager, active, nil)
				finished <- err
				return err
			}
			manager.SetRunner(ReloginUploadRunner(manager, operation, upload))
			manager.Start()
			if _, err := manager.QueueOperation(job.ID, "relogin", map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-workflow-admin"}}); err != nil {
				t.Fatal(err)
			}
			if scenario == "upload-first" {
				select {
				case <-mutationStarted:
				case err := <-finished:
					t.Fatalf("upload stopped before mutation: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("automatic upload did not reach the loopback mutation")
				}
				release, err := accountlock.Acquire(remote.URL, job.Email)
				if release != nil {
					_ = release()
				}
				if !errors.Is(err, accountlock.ErrBusy) {
					t.Fatalf("another instance acquired the account lock during upload: err=%v writes=%d", err, writes.Load())
				}
				finishMutation()
				select {
				case err := <-finished:
					if err != nil {
						t.Fatal("valid upload failed after releasing the loopback response", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("valid upload did not finish")
				}
				release, err = accountlock.Acquire(remote.URL, job.Email)
				if err != nil {
					t.Fatal("finished upload retained the OS account lock", err)
				}
				_ = release()
				return
			}
			select {
			case err := <-finished:
				var outcome interface{ UploadOutcomeUncertain() bool }
				if !errors.Is(err, accountlock.ErrBusy) || !errors.As(err, &outcome) || outcome.UploadOutcomeUncertain() || reads.Load() != 0 || writes.Load() != 0 {
					t.Fatalf("automatic upload bypassed another instance: err=%v reads=%d writes=%d", err, reads.Load(), writes.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("automatic upload did not finish")
			}
		})
	}
}

// The embedded in-memory store is a synthetic durable-store stand-in for the
// job state machine; this fixture never opens the real credential provider.
type strictWorkflowVault struct{ *credentials.MemoryStore }

func TestStrictWorkflowUploadRechecksRecoveryAndAllowsCompletedOperations(t *testing.T) {
	for _, operationName := range []string{"relogin", "setup-2fa", "reset-2fa"} {
		for _, scenario := range []string{"valid", "vault-after-authentication", "vault-after-quality"} {
			t.Run(operationName+"/"+scenario, func(t *testing.T) {
				var requests, mutations, qualityChecks atomic.Int32
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						_, _ = w.Write([]byte(`{"data":{"items":[],"pages":1,"total":0}}`))
						return
					}
					mutations.Add(1)
					_, _ = w.Write([]byte(`{"created":1}`))
				}))
				t.Cleanup(remote.Close)
				disk, err := store.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				manager := jobs.NewManager(t.Context(), 1, disk, nil)
				vault := &strictWorkflowVault{credentials.NewMemoryStore()}
				manager.SetCredentialStore(vault)
				t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
				job, err := manager.Submit(jobs.Spec{ID: "strict-workflow-boundary", Email: "workflow-boundary@example.test", ImportOnly: true, Input: map[string]any{"totpSecret": "JBSWY3DPEHPK3PXP"}})
				if err != nil {
					t.Fatal(err)
				}
				writePrivateGrantFixture(t, filepath.Join(disk.Root, job.ID, "sub2api-import-oauth.json"), `{"type":"sub2api-data","accounts":[{"platform":"openai","type":"oauth","credentials":{"email":"workflow-boundary@example.test","access_token":"synthetic-workflow-access"}}]}`)
				controller, err := NewAccountQualityController(config.Config{OutputRoot: disk.Root, ChatGPTBase: remote.URL}, manager)
				if err != nil {
					t.Fatal(err)
				}
				markPending := func() error {
					c, err := vault.Load(job.Email)
					if err != nil {
						return err
					}
					c.TOTPResetPending = true
					return vault.Save(job.Email, c)
				}
				manager.SetAuthorizationSyncCheck(func(ctx context.Context, id string, account map[string]any) error {
					qualityChecks.Add(1)
					release, lockErr := accountlock.Acquire(remote.URL, job.Email)
					if release != nil {
						_ = release()
					}
					if !errors.Is(lockErr, accountlock.ErrBusy) {
						return errors.New("automatic quality check lacks OS account ownership")
					}
					if err := controller.WorkflowUploadCheck(ctx, id, account); err != nil {
						return err
					}
					if scenario == "vault-after-quality" {
						return markPending()
					}
					return nil
				})
				operation := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
					release, err := accountlock.Acquire(remote.URL, active.Email)
					if err != nil {
						return err
					}
					defer release()
					if active.Operation == "relogin" {
						manager.UpdateResult(active.ID, true, "synthetic fresh authorization")
						return nil
					}
					if active.Operation == "reset-2fa" {
						intent := map[string]any{"version": 1, "operation_scope": "current_session", "email": active.Email, "operation_id": "strict-completed-operation", "account_id": "fixture-account", "reset_totp_requested": true, "reset_totp_phase": "quarantine_intent", "old_totp_factor_id": "old-factor", "old_factor_invalidated": false, "mutation_schema": 1, "disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent", "enroll_attempted": false, "activation_attempted": false}
						if err := protocol.WritePrivateJSON(filepath.Join(disk.Root, active.ID, "totp-setup-result.json"), intent); err != nil {
							return err
						}
						if err := manager.BeginTOTPReset(ctx, active.ID, "strict-completed-operation"); err != nil {
							return err
						}
					}
					c, err := vault.Load(active.Email)
					if err != nil {
						return err
					}
					c.TOTPSecret, c.TOTPResetPending = "KRSXG5DSNFXGOIDB", false
					if err := vault.Save(active.Email, c); err != nil {
						return err
					}
					if active.Operation != "reset-2fa" {
						manager.UpdateStoredCredentials(active.ID, c)
						return nil
					}
					writeCommittedResetFixture(t, disk.Root, manager, vault, active.ID, c.TOTPResetOperationID)
					if err := manager.CommitTOTPReset(ctx, active.ID, c); err != nil {
						return err
					}
					writeCommittedResetFixture(t, disk.Root, manager, vault, active.ID, c.TOTPResetOperationID)
					if err := manager.MarkTOTPResetCommitted(ctx, active.ID, c.TOTPResetOperationID); err != nil {
						return err
					}
					return nil
				}
				uploaded := make(chan error, 1)
				upload := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
					if scenario == "vault-after-authentication" {
						if err := markPending(); err != nil {
							return err
						}
					}
					err := UploadSavedAuthorizationAtOrigin(ctx, disk.Root, remote.URL, manager, active, nil)
					uploaded <- err
					return err
				}
				runner := ReloginUploadRunner(manager, TOTPWorkflowRunner(manager, vault, operation, upload), upload)
				finished := make(chan error, 1)
				manager.SetRunner(func(ctx context.Context, active *jobs.Job, emit func(jobs.JobEvent)) error {
					err := runner(ctx, active, emit)
					finished <- err
					return err
				})
				manager.Start()
				if _, err := manager.QueueOperation(job.ID, operationName, map[string]any{"uploadAfterRelogin": true, "reloginAfterTotp": true, "uploadAfterTotp": true, "sub2apiConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin"}}); err != nil {
					t.Fatal(err)
				}
				select {
				case runnerErr := <-finished:
					select {
					case uploadErr := <-uploaded:
						if scenario == "valid" {
							if runnerErr != nil || uploadErr != nil || requests.Load() != 2 || mutations.Load() != 1 || qualityChecks.Load() != 2 {
								t.Fatalf("completed workflow could not upload exactly once: runner=%v upload=%v requests=%d mutations=%d checks=%d", runnerErr, uploadErr, requests.Load(), mutations.Load(), qualityChecks.Load())
							}
						} else {
							var outcome interface{ UploadOutcomeUncertain() bool }
							if !errors.Is(uploadErr, jobs.ErrTOTPRecoveryPending) || !errors.As(uploadErr, &outcome) || outcome.UploadOutcomeUncertain() || requests.Load() != 0 {
								t.Fatalf("changed recovery evidence reached upstream or lost known-unsent status: err=%v requests=%d", uploadErr, requests.Load())
							}
						}
					default:
						t.Fatal("workflow stopped before automatic upload", runnerErr)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("automatic workflow did not finish")
				}
				if scenario == "vault-after-authentication" && qualityChecks.Load() != 0 {
					t.Fatal("unsafe post-authentication state reached the quality provider")
				}
				release, err := accountlock.Acquire(remote.URL, job.Email)
				if err != nil {
					t.Fatal("finished workflow retained its OS account lease", err)
				}
				_ = release()
			})
		}
	}
}

func TestStrictWorkflowUploadLockFailureHasSafeKnownOutcome(t *testing.T) {
	err := UploadSavedAuthorizationAtOrigin(t.Context(), t.TempDir(), "invalid-private-origin-canary", nil, &jobs.Job{Email: "synthetic@example.test"}, nil)
	var outcome interface{ UploadOutcomeUncertain() bool }
	if err == nil || !errors.As(err, &outcome) || outcome.UploadOutcomeUncertain() || strings.Contains(err.Error(), "private-origin-canary") {
		t.Fatal("OS lock preflight exposed private configuration or lost known-unsent status", err)
	}
}
