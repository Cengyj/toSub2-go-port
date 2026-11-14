package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
)

type failedSMSDeleteVault struct{ *lockedSMSVault }

func (*failedSMSDeleteVault) Delete(string) error { return errors.New("fixture deletion failure") }

func TestSMSFinalizationRestartNeverReplaysUnconfirmedAction(t *testing.T) {
	for _, complete := range []bool{false, true} {
		for _, lostResponse := range []bool{false, true} {
			t.Run(fmt.Sprintf("complete=%v/lost=%v", complete, lostResponse), func(t *testing.T) {
				srv, jm := newMonitorTestServer(t)
				vault := &failedSMSDeleteVault{&lockedSMSVault{values: map[string]credentials.Credentials{}}}
				srv.monitorVault = vault
				var requests atomic.Int32
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					status := "8"
					if complete {
						status = "6"
					}
					if r.URL.Query().Get("action") != "setStatus" || r.URL.Query().Get("status") != status || r.URL.Query().Get("id") != "fixture-order" {
						t.Error("unexpected provider mutation")
					}
					if lostResponse {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					if complete {
						fmt.Fprint(w, "ACCESS_ACTIVATION")
					} else {
						fmt.Fprint(w, "ACCESS_CANCEL")
					}
				}))
				defer remote.Close()
				job, err := jm.Submit(jobs.Spec{Email: "sms-action@example.test"})
				if err != nil {
					t.Fatal(err)
				}
				jm.UpdateSMS(job.ID, "smsbower", "SMSBower", "+12025550123", "submitted", "")
				provider := providers.NewSMSBowerProvider("fixture-key", remote.URL, "dr", "1")
				lease := &smsLease{provider: provider, lease: providers.NumberLease{ID: "fixture-order", Number: "+12025550123", Provider: "smsbower"}, cancel: func() {}}
				srv.smsMu.Lock()
				srv.smsLeases[job.ID] = lease
				srv.smsMu.Unlock()
				srv.finishSMS(job.ID, lease, complete, "")
				if requests.Load() != 1 {
					t.Fatalf("initial requests = %d", requests.Load())
				}
				saved, err := vault.Load(smsCredentialID(job.ID))
				if err != nil {
					t.Fatal("order must remain recoverable after failed local deletion", err)
				}
				var state privateSMSState
				if json.Unmarshal([]byte(saved.Password), &state) != nil || state.Action == "" {
					t.Fatal("missing pending order bookkeeping")
				}
				restarted := &Server{cfg: srv.cfg, jobs: jm, logger: srv.logger, monitorVault: vault}
				restarted.initializeSMS()
				if err := restarted.closeSMSContext(context.Background()); err != nil {
					t.Fatal(err)
				}
				if requests.Load() != 1 {
					t.Fatalf("restart repeated provider action: %d requests", requests.Load())
				}
				current, _ := jm.Get(job.ID)
				if lostResponse && (current.SMSStatus == "completed" || strings.Contains(current.SMSError, "重启恢复后重试")) {
					t.Fatal("unknown action was presented as confirmed or replayable")
				}
				if _, err := vault.Load(smsCredentialID(job.ID)); err != nil {
					t.Fatal("restart discarded provider order")
				}
			})
		}
	}
}
