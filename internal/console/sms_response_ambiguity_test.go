package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestLubanAmbiguousReceiptCannotAdvanceConsoleSMS(t *testing.T) {
	const leaseID = "18446744073709551617"
	validNumber := `{"code":0,"request_id":` + leaseID + `,"number":12025550123}`
	for _, tc := range []struct {
		name, phase, response string
	}{
		{"rejected-number", "number", `{"code":400,"code":0,"request_id":"fixture-order","number":12025550123}`},
		{"conflicting-order", "number", `{"code":0,"request_id":"first-order","request_id":"other-order","number":12025550123}`},
		{"conflicting-number", "number", `{"code":0,"request_id":"fixture-order","number":12025550123,"number":12025550124}`},
		{"conflicting-otp", "poll", `{"code":0,"sms_code":"123456","sms_code":"654321"}`},
		{"conflicting-nested-otp", "poll", `{"code":0,"sms_code":{"code":"123456","code":"654321"}}`},
		{"rejected-release", "release", `{"code":400,"code":0,"msg":"private-provider-detail"}`},
		{"compatible-deep-metadata", "valid", `{"code":0,"request_id":` + leaseID + `,"number":12025550123,"metadata":` + strings.Repeat(`{"nested":`, 80) + `0` + strings.Repeat(`}`, 80) + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, manager := newMonitorTestServer(t)
			srv.cfg.SMSPollIntervalMS = 5
			vault := &lockedSMSVault{values: map[string]credentials.Credentials{}}
			srv.monitorVault = vault
			var numberCalls, pollCalls, releaseCalls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/getNumber":
					numberCalls.Add(1)
					body := validNumber
					if tc.phase == "number" || tc.phase == "valid" {
						body = tc.response
					}
					fmt.Fprint(w, body)
				case "/getSms":
					pollCalls.Add(1)
					if r.URL.Query().Get("request_id") != leaseID {
						t.Error("SMS poll changed the exact leased order ID")
					}
					fmt.Fprint(w, tc.response)
				case "/setStatus":
					releaseCalls.Add(1)
					if r.URL.Query().Get("request_id") != leaseID || r.URL.Query().Get("status") != "reject" {
						t.Error("release changed the leased order or legacy action")
					}
					if tc.phase == "release" {
						fmt.Fprint(w, tc.response)
					} else {
						fmt.Fprint(w, `{"code":0}`)
					}
				default:
					t.Error("unexpected provider endpoint")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(remote.Close)
			// Stop the SMS owner before closing its synthetic upstream.
			t.Cleanup(srv.closeSMS)
			job, err := manager.Submit(jobs.Spec{ID: "ambiguous-sms", Email: "sms@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			manager.Emit(jobs.JobEvent{JobID: job.ID, Stage: "phone", Status: jobs.StatusWaitingInput})
			response := frontendRequest(srv, http.MethodPost, "/api/jobs/"+job.ID+"/sms-number", map[string]any{
				"providerId": "luban", "config": map[string]any{"apiKey": "synthetic-luban-key", "serviceId": "121949", "baseUrl": remote.URL},
			}, "test")
			if tc.phase == "number" {
				if response.Code != http.StatusBadGateway {
					t.Fatalf("ambiguous acquisition was published as success: HTTP %d", response.Code)
				}
				input, _ := manager.InputCopy(job.ID)
				if input["phone"] != nil || numberCalls.Load() != 1 || pollCalls.Load() != 0 || releaseCalls.Load() != 0 {
					t.Fatal("ambiguous acquisition submitted a number or performed another provider operation")
				}
				if _, err := vault.Load(smsCredentialID(job.ID)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("ambiguous order identity entered durable lease state")
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatal("valid lease could not be acquired", response.Code)
			}
			if state := loadTrashSMSState(t, vault, job.ID); state.Lease.ID != leaseID {
				t.Fatal("exact numeric lease identity was lost at persistence")
			}
			if tc.phase == "poll" {
				manager.Emit(jobs.JobEvent{JobID: job.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
				deadline := time.Now().Add(2 * time.Second)
				for {
					current, _ := manager.Get(job.ID)
					input, _ := manager.InputCopy(job.ID)
					if input["phone_otp"] != nil {
						t.Fatal("ambiguous provider response submitted an OTP to the login flow")
					}
					if current.SMSStatus == "error" {
						if current.Status != "phone_otp" {
							t.Fatal("provider ambiguity removed manual OTP input")
						}
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("ambiguous response did not stop automatic polling")
					}
					time.Sleep(time.Millisecond)
				}
				time.Sleep(30 * time.Millisecond)
				if pollCalls.Load() != 1 || releaseCalls.Load() != 0 {
					t.Fatal("ambiguous response was retried or the usable manual lease was released")
				}
				manual := frontendRequest(srv, http.MethodPost, "/api/jobs/"+job.ID+"/input", map[string]any{"action": "phone_otp", "value": "456789"}, "test")
				if manual.Code != http.StatusOK {
					t.Fatal("manual input was unavailable after automatic polling stopped")
				}
				return
			}
			srv.releaseSMS(job.ID)
			current, _ := manager.Get(job.ID)
			if releaseCalls.Load() != 1 {
				t.Fatal("release must dispatch exactly once")
			}
			if tc.phase == "release" {
				state := loadTrashSMSState(t, vault, job.ID)
				if current.SMSStatus != "error" || !strings.Contains(current.SMSError, "未确认") || state.Action != "release" || state.Lease.ID != leaseID {
					t.Fatal("ambiguous release was marked confirmed or lost its recovery identity")
				}
				if strings.Contains(current.SMSError, "private-provider-detail") {
					t.Fatal("provider response leaked into public diagnostics")
				}
			} else if _, err := vault.Load(smsCredentialID(job.ID)); !errors.Is(err, os.ErrNotExist) || current.SMSStatus != "idle" {
				t.Fatal("valid release did not retain the legacy completion behavior")
			}
		})
	}
}
