package console

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestCPAIncompatibleTOTPRequestDoesNotCancelPhoneOrReleaseSMS(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			s, manager := newMonitorTestServer(t)
			manager.SetCredentialStore(credentials.NewMemoryStore())
			saveCPAFixture(t, s, "https://cpa.example.test")
			job, err := manager.Submit(jobs.Spec{ID: "bps-phone", Email: "bps-phone@example.test", ImportOnly: true, Input: map[string]any{"authorizationMethod": "bps"}})
			if err != nil {
				t.Fatal(err)
			}
			// A compatible account comes first in the batch. Preflight must
			// reject the whole request before admitting its 2FA workflow.
			compatible := importTOTPActionFixture(t, manager, "compatible-first", false)
			ready := make(chan struct{})
			var cancellations atomic.Int32
			manager.SetRunner(func(ctx context.Context, active *jobs.Job, emit func(jobs.JobEvent)) error {
				if active.ID != job.ID {
					t.Error("invalid batch admitted another account")
					return nil
				}
				emit(jobs.JobEvent{Stage: "phone_otp", Status: jobs.StatusWaitingInput})
				close(ready)
				<-ctx.Done()
				cancellations.Add(1)
				return ctx.Err()
			})
			if _, err := manager.QueueOperation(job.ID, "relogin", nil); err != nil {
				t.Fatal(err)
			}
			manager.Start()
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("phone flow did not start")
			}
			provider := &smsTestProvider{}
			lease, _ := provider.GetNumber(t.Context())
			leaseCtx, cancelLease := context.WithCancel(t.Context())
			defer cancelLease()
			s.smsMu.Lock()
			s.smsLeases[job.ID] = &smsLease{provider: provider, lease: lease, cancel: cancelLease, seen: map[string]bool{}}
			s.smsMu.Unlock()
			before, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"reloginAfterTotp": true, "uploadAfterTotp": true, "uploadTarget": "cpa"}
			path := "/api/jobs/" + job.ID + "/setup-2fa"
			if batch {
				path, body["ids"] = "/api/jobs/setup-2fa-batch", []string{compatible.ID, job.ID}
			}
			response := frontendRequest(s, http.MethodPost, path, body, "test")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "BPS") {
				t.Fatal("incompatible inherited authorization was not rejected", response.Code, response.Body.String())
			}
			state, _ := manager.Get(job.ID)
			other, _ := manager.Get(compatible.ID)
			after, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
			if err != nil || string(before) != string(after) || state.Status != "phone_otp" || state.Operation != "relogin" || state.AuthorizationMethod != "bps" || other.Status != jobs.StatusImported || manager.QueuePosition(compatible.ID) != 0 {
				t.Fatal("CPA preflight changed an existing task or partially admitted the batch", err)
			}
			if cancellations.Load() != 0 || provider.releases.Load() != 0 || leaseCtx.Err() != nil {
				t.Fatal("rejected CPA request canceled authentication or released its SMS order")
			}
			if !manager.InputForStage(job.ID, "phone_otp", map[string]any{"phone_otp": "112233"}) {
				t.Fatal("existing SMS prompt no longer accepts its answer")
			}
			s.smsMu.Lock()
			delete(s.smsLeases, job.ID)
			s.smsMu.Unlock()
		})
	}
}
