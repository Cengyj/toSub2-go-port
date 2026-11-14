package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
)

type trashSMSVault struct {
	lockedSMSVault
	failSave, failDelete, blockSave atomic.Bool
}

func (v *trashSMSVault) LoadContext(ctx context.Context, id string) (credentials.Credentials, error) {
	if err := ctx.Err(); err != nil {
		return credentials.Credentials{}, err
	}
	return v.Load(id)
}
func (v *trashSMSVault) SaveContext(ctx context.Context, id string, value credentials.Credentials) error {
	if v.blockSave.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	if v.failSave.Load() {
		return errors.New("synthetic-private-save-error")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.Save(id, value)
}
func (v *trashSMSVault) DeleteContext(ctx context.Context, id string) error {
	if v.failDelete.Load() {
		return errors.New("synthetic-private-delete-error")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.Delete(id)
}

func installTrashSMSFixture(t *testing.T, srv *Server, jm *jobs.Manager, remote string) (*jobs.Job, *smsLease) {
	t.Helper()
	job, err := jm.Submit(jobs.Spec{ID: "sms-trash", Email: "sms-trash@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	jm.Emit(jobs.JobEvent{JobID: job.ID, Status: "resume_available"})
	jm.UpdateSMS(job.ID, "smsbower", "SMSBower", "+12025550123", "waiting_sms", "")
	p := providers.NewSMSBowerProvider("synthetic-key", remote, "dr", "1")
	if !srv.installSMSLease(job.ID, p, providers.NumberLease{ID: "synthetic-lease", Provider: "smsbower", Number: "+12025550123"}) {
		t.Fatal("lease installation failed")
	}
	srv.smsMu.Lock()
	lease := srv.smsLeases[job.ID]
	lease.pendingCode = "654321"
	lease.seen["654321"] = true
	srv.smsMu.Unlock()
	if err := srv.saveSMSLease(job.ID, lease); err != nil {
		t.Fatal(err)
	}
	job, _ = jm.Get(job.ID)
	return job, lease
}

func loadTrashSMSState(t *testing.T, vault credentials.Store, id string) privateSMSState {
	t.Helper()
	stored, err := vault.Load(smsCredentialID(id))
	if err != nil {
		t.Fatal(err)
	}
	var state privateSMSState
	if err := json.Unmarshal([]byte(stored.Password), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSMSTrashPauseRetainsSecretsAndNeverReplaysAfterRestore(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	vault := &trashSMSVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}}
	srv.monitorVault = vault
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "ACCESS_CANCEL")
	}))
	defer remote.Close()
	job, lease := installTrashSMSFixture(t, srv, jm, remote.URL)
	if err := srv.pauseTrashSMS(t.Context(), []*jobs.Job{job}); err != nil {
		t.Fatal(err)
	}
	srv.smsMu.Lock()
	owned, gated := srv.smsLeases[job.ID] != nil, srv.smsTrashing[job.ID]
	srv.smsMu.Unlock()
	if owned || !gated {
		t.Fatal("successful pause retained ownership or released the deletion fence early")
	}
	state := loadTrashSMSState(t, vault, job.ID)
	if !state.TrashPaused || state.Email != job.Email || state.APIKey != "synthetic-key" {
		t.Fatal("provider order credentials were discarded or identity lost")
	}
	srv.finishSMS(job.ID, lease, true, "")
	srv.releaseSMS(job.ID)
	srv.resumeTrashSMS([]string{job.ID})
	restarted := &Server{cfg: srv.cfg, jobs: jm, monitorVault: vault}
	restarted.initializeSMS()
	t.Cleanup(restarted.closeSMS)
	restarted.smsMu.Lock()
	installed := len(restarted.smsLeases)
	restarted.smsMu.Unlock()
	if installed != 0 || requests.Load() != 0 {
		t.Fatal("restoration replayed SMS polling or remote finalization")
	}
	if !loadTrashSMSState(t, vault, job.ID).TrashPaused {
		t.Fatal("startup removed the archived recovery record")
	}
}

func TestSMSTrashPauseFailureKeepsOwnershipAndPrivateHistory(t *testing.T) {
	for _, kind := range []string{"save", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			srv, jm := newMonitorTestServer(t)
			vault := &trashSMSVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}}
			srv.monitorVault = vault
			job, lease := installTrashSMSFixture(t, srv, jm, "http://127.0.0.1:1")
			before, _ := vault.Load(smsCredentialID(job.ID))
			if kind == "save" {
				vault.failSave.Store(true)
			} else {
				vault.blockSave.Store(true)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			err := srv.pauseTrashSMS(ctx, []*jobs.Job{job})
			if !errors.Is(err, errSMSTrashPersistence) || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("private failure leaked or was ignored", err)
			}
			if kind == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("caller deadline lost", err)
			}
			srv.smsMu.Lock()
			kept, gated := srv.smsLeases[job.ID] == lease, srv.smsTrashing[job.ID]
			srv.smsMu.Unlock()
			after, _ := vault.Load(smsCredentialID(job.ID))
			if !kept || gated || before.Password != after.Password {
				t.Fatal("failed pause changed ownership or private history")
			}
			vault.failSave.Store(false)
			vault.blockSave.Store(false)
		})
	}
}

func TestSMSTrashPauseRejectsAcquisitionAndFinishingOwnership(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	vault := &trashSMSVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}}
	srv.monitorVault = vault
	entered, release := make(chan struct{}), make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "ACCESS_CANCEL")
	}))
	defer remote.Close()
	job, lease := installTrashSMSFixture(t, srv, jm, remote.URL)
	srv.smsMu.Lock()
	srv.smsAcquiring[job.ID] = true
	srv.smsMu.Unlock()
	if err := srv.pauseTrashSMS(t.Context(), []*jobs.Job{job}); !errors.Is(err, errSMSTrashBusy) {
		t.Fatal("acquisition was not protected", err)
	}
	srv.smsMu.Lock()
	delete(srv.smsAcquiring, job.ID)
	srv.smsMu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); srv.finishSMS(job.ID, lease, false, "") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("finish request never started")
	}
	err := srv.pauseTrashSMS(t.Context(), []*jobs.Job{job})
	close(release)
	if !errors.Is(err, errSMSTrashBusy) {
		t.Fatal("in-flight finalization was not protected", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("finish did not release ownership")
	}
}

func TestSMSTrashPauseArchivesSavedActionWithoutReplaying(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	vault := &trashSMSVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}}
	srv.monitorVault = vault
	job, err := jm.Submit(jobs.Spec{ID: "archived-action", Email: "archived-action@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	jm.UpdateSMS(job.ID, "smsbower", "SMSBower", "+12025550123", "error", "synthetic failure")
	state := privateSMSState{Version: 1, JobID: job.ID, Email: job.Email, Provider: "smsbower", Lease: providers.NumberLease{ID: "pending", Provider: "smsbower", Number: "+12025550123"}, Action: "release", APIKey: "synthetic-key"}
	if err := srv.writeSMSState(state); err != nil {
		t.Fatal(err)
	}
	job, _ = jm.Get(job.ID)
	if err := srv.pauseTrashSMS(t.Context(), []*jobs.Job{job}); err != nil {
		t.Fatal(err)
	}
	got := loadTrashSMSState(t, vault, job.ID)
	if !got.TrashPaused || got.Action != "release" || got.APIKey != "synthetic-key" {
		t.Fatal("pending action was not preserved as a paused archive")
	}
}

func TestSMSTrashPurgeDeletesOnlyLocalSelectedJobRecordsAndRetries(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	vault := &trashSMSVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}}
	srv.monitorVault = vault
	for _, id := range []string{smsCredentialID("purged"), smsCredentialID("other"), "sms-pool-shared"} {
		if err := vault.Save(id, credentials.Credentials{Password: "synthetic-private-state"}); err != nil {
			t.Fatal(err)
		}
	}
	entry := &jobs.TrashEntry{State: jobs.TrashStatePurging, Jobs: []*jobs.Job{{ID: "purged"}}}
	vault.failDelete.Store(true)
	if err := srv.purgeTrashSMS(t.Context(), entry); !errors.Is(err, errSMSTrashPurge) || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("delete failure ignored or leaked", err)
	}
	if _, err := vault.Load(smsCredentialID("purged")); err != nil {
		t.Fatal("failed deletion lost recovery state", err)
	}
	vault.failDelete.Store(false)
	if err := srv.purgeTrashSMS(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	if err := srv.purgeTrashSMS(t.Context(), entry); err != nil {
		t.Fatal("local purge is not idempotent", err)
	}
	if _, err := vault.Load(smsCredentialID("purged")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("selected record retained", err)
	}
	for _, id := range []string{smsCredentialID("other"), "sms-pool-shared"} {
		if _, err := vault.Load(id); err != nil {
			t.Fatal("unrelated shared record deleted", err)
		}
	}
	job, err := jm.Submit(jobs.Spec{ID: "active", Email: "active@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	entry.Jobs = []*jobs.Job{job}
	if err := srv.purgeTrashSMS(t.Context(), entry); !errors.Is(err, errSMSTrashBusy) {
		t.Fatal("active account purge accepted", err)
	}
}
