package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func strictReviewUploadPayload(email string) map[string]any {
	return map[string]any{"accounts": []any{map[string]any{"name": email, "platform": "openai", "type": "oauth", "credentials": map[string]any{"email": email, "access_token": "strict-review-access", "refresh_token": "strict-review-refresh"}}}}
}

func TestStrictTOTPManualUploadStopsWhenResetStartsAfterPreflight(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected-ids", true: "raw-without-ids"}[raw], func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			m.SetCredentialStore(&monitorMemoryVault{values: map[string]credentials.Credentials{}})
			job := importTOTPActionFixture(t, m, "strict-upload-race", true)
			if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
				t.Fatal(err)
			}
			m.UpdateResult(job.ID, true, "synthetic saved authorization")
			lookup, release, resetEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var lookupOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var writes atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					lookupOnce.Do(func() { close(lookup) })
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []any{}, "pages": 1}})
					return
				}
				writes.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"created": 1}})
			}))
			t.Cleanup(func() { unblock(); remote.Close() })
			m.SetRunner(func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				if err := beginConsoleTOTPReset(t, s.cfg.OutputRoot, m, ctx, active.ID, "strict-review-operation"); err != nil {
					return err
				}
				close(resetEntered)
				return errors.New("synthetic reset stopped before remote work")
			})
			m.Start()
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			body := map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin-key"}
			if raw {
				body["payload"] = strictReviewUploadPayload(job.Email)
			} else {
				body["ids"] = []string{job.ID}
			}
			completed := make(chan *httptest.ResponseRecorder, 1)
			go func() { completed <- frontendRequest(s, http.MethodPost, "/api/sub2api/upload", body, "test") }()
			select {
			case <-lookup:
			case <-time.After(5 * time.Second):
				t.Fatal("upload never reached the read-only lookup after preflight")
			}
			if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			var response *httptest.ResponseRecorder
			select {
			case <-resetEntered:
				if err := m.CheckTOTPExport(job.ID); !errors.Is(err, jobs.ErrTOTPRecoveryRequired) {
					t.Fatal("reset did not establish recovery quarantine", err)
				}
				unblock()
				select {
				case response = <-completed:
				case <-time.After(5 * time.Second):
					t.Fatal("upload did not finish after lookup release")
				}
			case response = <-completed:
				unblock()
			case <-time.After(5 * time.Second):
				t.Fatal("neither reset admission canceled upload nor reset acquired the account")
			}
			if writes.Load() != 0 || response.Code < 400 {
				t.Fatalf("preflight released account before upload mutation: writes=%d status=%d body=%s", writes.Load(), response.Code, response.Body.String())
			}
		})
	}
}

type strictReviewSignalVault struct {
	credentials.Store
	once sync.Once
	seen chan struct{}
}

func (v *strictReviewSignalVault) Load(email string) (credentials.Credentials, error) {
	value, err := v.Store.Load(email)
	v.once.Do(func() { close(v.seen) })
	return value, err
}

func TestStrictTOTPBatchDownloadRechecksCurrentGrantAfterFileRead(t *testing.T) {
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("named pipes unavailable on this test host")
	}
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	job := importTOTPActionFixture(t, m, "strict-batch-grant", false)
	if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "synthetic saved authorization")
	path := filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("mkfifo", path).Run(); err != nil {
		t.Fatal(err)
	}
	pipe, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pipe.Close() })
	seen := make(chan struct{})
	m.SetCredentialStore(&strictReviewSignalVault{Store: vault, seen: seen})
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		completed <- frontendRequest(s, http.MethodPost, "/api/jobs/download-batch", map[string]any{"ids": []string{job.ID}}, "test")
	}()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("batch download never checked its selected snapshot")
	}
	m.UpdateResult(job.ID, false, "authorization invalidated during file read")
	if _, err := pipe.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := pipe.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-completed:
		if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "synthetic-access") {
			t.Fatalf("batch returned a grant invalidated after selection: status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("batch download did not finish after synthetic file read")
	}
}

func TestStrictTOTPDownloadsRejectMismatchedAccountExport(t *testing.T) {
	s, m, pending, _ := recoveryAPIWithVault(t)
	allowed := importTOTPActionFixture(t, m, "strict-unrelated-selection", false)
	if err := writeFlowExport(s.cfg.OutputRoot, allowed.ID, pending.Email); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(allowed.ID, true, "synthetic mismatched cached export")
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/jobs/" + allowed.ID + "/download", nil},
		{http.MethodPost, "/api/jobs/download-batch", map[string]any{"ids": []string{allowed.ID}}},
	} {
		response := frontendRequest(s, request.method, request.path, request.body, "test")
		if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "synthetic-access") {
			t.Errorf("selection gate released another quarantined account's cached grant: route=%s status=%d body=%s", request.path, response.Code, response.Body.String())
		}
	}
}

func TestStrictTOTPManualUploadLeaseWaitsForUncooperativeConsumer(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(&monitorMemoryVault{values: map[string]credentials.Credentials{}})
	job := importTOTPActionFixture(t, m, "strict-upload-owner", true)
	marker := importTOTPActionFixture(t, m, "strict-unrelated-marker", false)
	ctx, check, releaseLease := s.quality.NewUploadSession(t.Context(), []string{job.ID})
	account := strictReviewUploadPayload(job.Email)["accounts"].([]any)[0].(map[string]any)
	if err := check(ctx, account); err != nil {
		t.Fatal(err)
	}
	finishRead, consumerDone := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finishRead) }) }
	go func() { <-finishRead; releaseLease(); close(consumerDone) }()
	t.Cleanup(func() { finish(); <-consumerDone; _ = m.Shutdown(context.Background()) })
	resetEntered, markerEntered := make(chan struct{}), make(chan struct{})
	m.SetRunner(func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
		if active.ID == job.ID {
			close(resetEntered)
		} else {
			close(markerEntered)
		}
		return nil
	})
	if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(marker.ID, "relogin", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("reset admission did not cancel the upload session")
	}
	m.Start()
	select {
	case <-markerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not advance unrelated account work")
	}
	select {
	case <-resetEntered:
		t.Fatal("cancellation released ownership while the upload consumer was still active")
	default:
	}
	if _, err := m.ReserveAccountQuery(t.Context(), job.ID); !errors.Is(err, jobs.ErrAccountQueryBusy) {
		t.Fatal("canceled but unreleased upload lost its account lease", err)
	}
	finish()
	select {
	case <-resetEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not start after the upload consumer released ownership")
	}
}

func TestStrictTOTPValidatedSingleDownloadPreservesOriginalVendorBytes(t *testing.T) {
	s, m := newMonitorTestServer(t)
	job := importTOTPActionFixture(t, m, "strict-vendor-export", false)
	if err := writeFlowExport(s.cfg.OutputRoot, job.ID, job.Email); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{ "type": "sub2api-data", "version":1, "accounts":[{"credentials":{"email":"strict-vendor-export@example.test","access_token":"synthetic-access"},"vendor_id":18446744073709551615}], "proxies":[] }` + "\n")
	if err := os.WriteFile(filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "synthetic authorization")
	response := frontendRequest(s, http.MethodGet, "/api/jobs/"+job.ID+"/download", nil, "test")
	if response.Code != http.StatusOK || response.Body.String() != string(data) {
		t.Fatal("identity validation rewrote vendor data", response.Code, response.Body.String())
	}
}

type strictReviewUnavailableConsoleVault struct{ credentials.Store }

func (v *strictReviewUnavailableConsoleVault) Load(string) (credentials.Credentials, error) {
	return credentials.Credentials{}, errors.New("synthetic inaccessible vault: /fixture/private-canary")
}

func TestStrictTOTPSharedUploadSessionChecksVaultWithQualityDisabled(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	job := importTOTPActionFixture(t, m, "strict-shared-upload", true)
	m.SetCredentialStore(&strictReviewUnavailableConsoleVault{Store: vault})
	ctx, check, release := s.quality.NewUploadSession(t.Context(), []string{job.ID})
	defer release()
	account := strictReviewUploadPayload(job.Email)["accounts"].([]any)[0].(map[string]any)
	if err := check(ctx, account); !errors.Is(err, jobs.ErrTOTPExportUnavailable) || strings.Contains(err.Error(), "private-canary") {
		t.Fatal("shared upload session bypassed unavailable configured vault", err)
	}
}
