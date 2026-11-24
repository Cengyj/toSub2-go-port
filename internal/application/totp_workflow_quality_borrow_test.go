package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

// Keep the real reset, follow-up, quality and upload state machines in one
// active scheduler operation. Only authentication receipts and network replies
// are synthetic; no real factor, account or inference service is contacted.
func TestCompletedActiveResetQualityUploadBorrowsOwnership(t *testing.T) {
	f, controller := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	vault := &strictWorkflowVault{f.vault}
	f.manager.SetCredentialStore(vault)
	saved, err := vault.Load(f.job.Email)
	if err != nil {
		t.Fatal(err)
	}
	saved.TOTPSecret, saved.TOTPFactorID = "JBSWY3DPEHPK3PXP", "old-factor"
	if err := vault.Save(f.job.Email, saved); err != nil {
		t.Fatal(err)
	}
	f.manager.UpdateStoredCredentials(f.job.ID, saved)
	enableQualityFixture(t, controller, nil)
	// The explicit empty override must remain a direct connection even with a
	// global proxy configured; it also exercises the borrowed non-vault branch.
	controller.cfg.ProxyURL = "http://unused-global-proxy.synthetic.invalid:8080"
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	if err := privatestate.Register(f.store.Root, protector); err != nil {
		t.Fatal(err)
	}
	const operationID = "completed-active-reset-quality"
	const newSeed = "KRSXG5DSNFXGOIDB"
	const newAccess = "synthetic-fresh-reset-access"
	journalPath := filepath.Join(f.store.Root, f.job.ID, "totp-setup-result.json")
	var sequenceMu sync.Mutex
	var sequence []string
	record := func(event string) {
		sequenceMu.Lock()
		sequence = append(sequence, event)
		sequenceMu.Unlock()
	}
	var operations, operationReleases, gates, probes, reads, writes, reentries atomic.Int32
	var openedTransports, closedTransports, admissionLeases, admissionReleases atomic.Int32
	var workflowStarted atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if probes.Load() != 1 {
			t.Error("upload reached the target before one successful candy probe")
			http.Error(w, "invalid probe order", http.StatusConflict)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			reads.Add(1)
			record("discover")
			_, _ = io.WriteString(w, `{"data":{"items":[],"pages":1,"total":0}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/batch":
			writes.Add(1)
			record("upload")
			var payload struct {
				Accounts []struct {
					Credentials map[string]any `json:"credentials"`
				} `json:"accounts"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload) != nil || len(payload.Accounts) != 1 ||
				payload.Accounts[0].Credentials["email"] != f.job.Email || payload.Accounts[0].Credentials["access_token"] != newAccess ||
				payload.Accounts[0].Credentials["chatgpt_account_id"] != "synthetic-workspace" || payload.Accounts[0].Credentials["client_id"] != protocol.DefaultCodexClientID {
				t.Error("upload did not use the fresh account-bound authorization")
				http.Error(w, "invalid authorization", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"created":1}`)
		default:
			t.Error("unexpected upload method or route", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(remote.Close)
	f.manager.SetAccountOperationLease(func(email string) (func() error, error) {
		if workflowStarted.Load() {
			reentries.Add(1)
			return nil, errors.New("borrowed workflow must not acquire a query lease")
		}
		// Queueing an explicit proxy edit now owns the same host-wide account
		// lease. Only this pre-execution admission may acquire it normally.
		release, err := accountlock.Acquire(remote.URL, email)
		if err != nil {
			return nil, err
		}
		admissionLeases.Add(1)
		return func() error { admissionReleases.Add(1); return release() }, nil
	})
	assertOwned := func(ctx context.Context) error {
		active, ok := f.manager.Get(f.job.ID)
		if !ok || !f.manager.ActiveAccountOperation(f.job.ID) || active.Operation != "reset-2fa" ||
			active.TOTPWorkflowStep != "upload" || !active.TOTPResetCommitted || active.TOTPRecoveryPending ||
			active.TOTPCredentialInvalidated || f.manager.ActivitySnapshot().Queries != 0 {
			return errors.New("quality upload lost its completed active reset ownership")
		}
		// This control must reject: blindly adding the ordinary credential gate
		// to a borrowed grant would incorrectly block its own active reset.
		if _, err := f.manager.LoadCredentialsContext(ctx, f.job.ID); !errors.Is(err, jobs.ErrTOTPRecoveryRequired) {
			return errors.New("fixture no longer distinguishes an active reset from an ordinary query")
		}
		peerRelease, err := accountlock.Acquire(remote.URL, f.job.Email)
		if peerRelease != nil {
			_ = peerRelease()
		}
		if !errors.Is(err, accountlock.ErrBusy) {
			return errors.New("quality upload released the host-wide account lock")
		}
		return nil
	}
	controller.transportFactory = func(proxy string) (protocol.Doer, func(), error) {
		if proxy != "" {
			return nil, nil, errors.New("explicit direct proxy override was lost")
		}
		openedTransports.Add(1)
		return qualityDoer(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
			if err := assertOwned(ctx); err != nil {
				return nil, err
			}
			if request.Method != http.MethodPost || request.Headers.Get("Authorization") != "Bearer "+newAccess || request.Headers.Get("ChatGPT-Account-ID") != "synthetic-workspace" {
				return nil, errors.New("candy probe used the wrong committed authorization")
			}
			probes.Add(1)
			record("probe")
			return qualitySyntheticResponse(request, true), nil
		}), func() { closedTransports.Add(1) }, nil
	}
	f.manager.SetAuthorizationSyncCheck(func(ctx context.Context, id string, account map[string]any) error {
		gates.Add(1)
		record("quality")
		if err := assertOwned(ctx); err != nil {
			return err
		}
		return controller.WorkflowUploadCheck(ctx, id, account)
	})
	operation := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
		release, err := accountlock.Acquire(remote.URL, active.Email)
		if err != nil {
			return err
		}
		operations.Add(1)
		defer func() { _ = release(); operationReleases.Add(1) }()
		if active.Operation == "relogin" {
			current, err := vault.Load(active.Email)
			if err != nil || current.TOTPSecret != newSeed || current.TOTPResetPending || current.TOTPResetOperationID != operationID {
				return errors.New("relogin did not receive the committed new factor")
			}
			f.account["credentials"].(map[string]any)["access_token"] = newAccess
			if err := protocol.WritePrivateJSON(f.path, f.document); err != nil {
				return err
			}
			f.manager.UpdateResult(active.ID, true, "synthetic fresh authorization")
			record("reauthorized")
			return nil
		}
		if active.Operation != "reset-2fa" {
			return errors.New("unexpected operation in reset workflow")
		}
		intent := map[string]any{
			"version": 1, "operation_scope": "current_session", "email": active.Email, "operation_id": operationID,
			"account_id": "fixture-account", "reset_totp_requested": true, "reset_totp_phase": "quarantine_intent",
			"old_totp_factor_id": "old-factor", "old_factor_invalidated": false, "mutation_schema": 1,
			"disable_dispatch": "not_sent", "enroll_dispatch": "not_sent", "activation_dispatch": "not_sent",
			"enroll_attempted": false, "activation_attempted": false,
		}
		if err := privatestate.WriteJSON(ctx, journalPath, "totp-reset", active.Email, operationID, intent); err != nil {
			return err
		}
		if err := f.manager.BeginTOTPReset(ctx, active.ID, operationID); err != nil {
			return err
		}
		current, err := vault.Load(active.Email)
		if err != nil {
			return err
		}
		current.TOTPSecret, current.TOTPFactorID, current.TOTPResetPending = newSeed, "fixture-new", false
		if err := vault.Save(active.Email, current); err != nil {
			return err
		}
		completion := map[string]any{
			"version": 1, "operation_scope": "current_session", "email": active.Email, "operation_id": operationID, "account_id": "fixture-account",
			"reset_totp_requested": true, "old_factor_invalidated": true, "old_totp_factor_id": "old-factor", "reset_totp_attempted_at": "2026-11-07T00:00:00Z",
			"enroll_attempted": true, "activation_attempted": true, "enrollment_validated": true, "secret": newSeed,
			"session_id": "fixture-session", "new_totp_factor_id": "fixture-new",
			"enrollment_factor":    map[string]any{"id": "fixture-new", "factor_type": "totp", "is_recovery": false},
			"activation_succeeded": true, "activation_verified": true, "activation_confirmed_by_read": true,
			"credential_persisted": true, "reset_totp_phase": "credential_persisted",
		}
		if err := privatestate.WriteJSON(ctx, journalPath, "totp-reset", active.Email, operationID, completion); err != nil {
			return err
		}
		if err := f.manager.CommitTOTPReset(ctx, active.ID, current); err != nil {
			return err
		}
		if err := f.manager.MarkTOTPResetCommitted(ctx, active.ID, operationID); err != nil {
			return err
		}
		record("reset-committed")
		return nil
	}
	upload := func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
		return UploadSavedAuthorizationAtOrigin(ctx, f.store.Root, remote.URL, f.manager, active, nil)
	}
	runner := TOTPWorkflowRunner(f.manager, vault, operation, upload)
	finished := make(chan error, 1)
	f.manager.SetRunner(func(ctx context.Context, active *jobs.Job, emit func(jobs.JobEvent)) error {
		workflowStarted.Store(true)
		err := runner(ctx, active, emit)
		finished <- err
		return err
	})
	events, unsubscribe := f.manager.Subscribe(64)
	defer unsubscribe()
	f.manager.Start()
	if _, err := f.manager.QueueOperation(f.job.ID, "reset-2fa", map[string]any{
		"proxyUrl": "", "reloginAfterTotp": true, "uploadAfterTotp": true,
		"sub2apiConfig": map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("completed active reset could not pass the enabled quality gate and upload", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("quality upload recursively waited for its own active account")
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for current, _ := f.manager.Get(f.job.ID); current.Status != jobs.StatusSucceeded; current, _ = f.manager.Get(f.job.ID) {
		select {
		case <-events:
		case <-deadline.C:
			t.Fatal("completed reset workflow did not release scheduler execution")
		}
	}
	sequenceMu.Lock()
	gotSequence := append([]string(nil), sequence...)
	sequenceMu.Unlock()
	wantSequence := []string{"reset-committed", "reauthorized", "quality", "probe", "discover", "quality", "upload"}
	if admissionLeases.Load() != 1 || admissionReleases.Load() != 1 {
		t.Fatalf("credential edit admission ownership changed: leases=%d releases=%d", admissionLeases.Load(), admissionReleases.Load())
	}
	if !reflect.DeepEqual(gotSequence, wantSequence) || operations.Load() != 2 || operationReleases.Load() != 2 || gates.Load() != 2 || probes.Load() != 1 || reads.Load() != 1 || writes.Load() != 1 || reentries.Load() != 0 || openedTransports.Load() != closedTransports.Load() {
		t.Fatalf("workflow order, request count or lease lifetime changed: sequence=%v operations=%d releases=%d gates=%d probes=%d reads=%d writes=%d reentries=%d transports=%d/%d", gotSequence, operations.Load(), operationReleases.Load(), gates.Load(), probes.Load(), reads.Load(), writes.Load(), reentries.Load(), openedTransports.Load(), closedTransports.Load())
	}
	current, _ := f.manager.Get(f.job.ID)
	retained, err := vault.Load(f.job.Email)
	if err != nil || !current.TOTPResetCommitted || !current.TOTPActivationVerified || current.TOTPRecoveryPending || current.TOTPCredentialInvalidated ||
		current.TOTPWorkflowStatus != "succeeded" || current.TOTPReloginStatus != "succeeded" || current.TOTPUploadStatus != "succeeded" || !current.ResultSaved ||
		retained.TOTPSecret != newSeed || retained.TOTPFactorID != "fixture-new" || retained.TOTPResetPending || retained.TOTPResetOperationID != operationID {
		t.Fatal("quality upload discarded the committed reset, new factor or independent step results", err)
	}
	rows, _ := controller.Service.Snapshots([]string{f.job.ID})
	row := rows[f.job.ID]
	if row.LastAttempt.Status != "success" || row.LastAttempt.Source != "upload" || !row.Persisted || row.LastResult == nil || row.LastResult.RuleVersion != "candy-v1" || row.LastResult.Score != 1 || row.LastResult.Total != 1 {
		t.Fatal("upload did not persist a real successful single-question probe")
	}
	journal, err := os.ReadFile(journalPath)
	proof, proofErr := protocol.ReadTOTPResetCompletion(journalPath, f.job.Email)
	if err != nil || proofErr != nil || bytes.Contains(journal, []byte(newSeed)) || bytes.Contains(journal, []byte(f.job.Email)) || proof["operation_id"] != operationID || proof["secret"] != newSeed {
		t.Fatal("workflow lost or exposed protected reset completion evidence", err, proofErr)
	}
	release, err := accountlock.Acquire(remote.URL, f.job.Email)
	if err != nil {
		t.Fatal("finished workflow retained the upload OS lock", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
