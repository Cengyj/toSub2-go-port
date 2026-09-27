package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
)

func TestSMSExplicitZeroDistinguishedFromUninitializedConfig(t *testing.T) {
	srv := &Server{}
	if srv.smsPollTimeout() != 10*time.Minute || srv.smsPollInterval() != 3*time.Second {
		t.Fatal("zero-value Config changed defaults")
	}
	srv.cfg.SMSPollTimeoutSet, srv.cfg.SMSPollIntervalSet = true, true
	if srv.smsPollTimeout() != 0 || srv.smsPollInterval() != time.Millisecond {
		t.Fatal("explicit zero discarded")
	}
	srv.cfg.SMSPollTimeoutMS, srv.cfg.SMSPollIntervalMS = -1, -1
	if srv.smsPollTimeout() != -time.Millisecond || srv.smsPollInterval() != time.Millisecond {
		t.Fatal("legacy nonpositive semantics changed")
	}
}

func TestSMSZeroTimeoutKeepsManualResendAndCompletion(t *testing.T) {
	for _, envName := range []string{"SMS_POLL_TIMEOUT_MS", "LUBAN_SMS_POLL_TIMEOUT_MS"} {
		t.Run(envName, func(t *testing.T) {
			t.Setenv("SMS_POLL_TIMEOUT_MS", "")
			t.Setenv("LUBAN_SMS_POLL_TIMEOUT_MS", "")
			t.Setenv(envName, "0")
			t.Setenv("SMS_POLL_INTERVAL_MS", "0")
			cfg, err := config.Parse(nil)
			if err != nil {
				t.Fatal(err)
			}
			srv, jm, base := prepareSMSFlow(t, 2000)
			srv.cfg.SMSPollTimeoutMS, srv.cfg.SMSPollTimeoutSet = cfg.SMSPollTimeoutMS, cfg.SMSPollTimeoutSet
			srv.cfg.SMSPollIntervalMS, srv.cfg.SMSPollIntervalSet = cfg.SMSPollIntervalMS, cfg.SMSPollIntervalSet
			var polls, releases, completes, resends atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Query().Get("action") {
				case "getNumber":
					fmt.Fprint(w, "ACCESS_NUMBER:zero-timeout-order:12025550123")
				case "getStatus":
					polls.Add(1)
					fmt.Fprint(w, "STATUS_OK:654321")
				case "setStatus":
					switch r.URL.Query().Get("status") {
					case "8":
						releases.Add(1)
					case "6":
						completes.Add(1)
					}
					fmt.Fprint(w, "ACCESS_READY")
				}
			}))
			defer remote.Close()
			jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
				emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
				if _, err := waitConsoleInput(ctx, jm, j.ID, "phone"); err != nil {
					return err
				}
				for _, want := range []string{"r", "123456"} {
					emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusWaitingInput})
					input, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
					if err != nil || input != want {
						return errors.New("zero-timeout manual input/resend lost")
					}
					if input == "r" {
						resends.Add(1)
					}
				}
				emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning})
				return nil
			})
			jm.Start()
			job, err := jm.Submit(jobs.Spec{ID: "zero-timeout", Email: "owner@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			waitConsoleState(t, jm, job.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
			acquireSMSFlow(t, base, job.ID, remote.URL)
			waitConsoleState(t, jm, job.ID, func(j *jobs.Job) bool { return j.Status == "phone_otp" && strings.Contains(j.SMSError, "超时") })
			if polls.Load() != 0 || releases.Load() != 0 {
				t.Fatal("explicit zero automatically polled or released number")
			}
			status, body := consoleHTTPPost(t, base, "/api/jobs/"+job.ID+"/input", map[string]any{"action": "resend_phone"})
			if status != http.StatusOK {
				t.Fatal(status, body)
			}
			waitConsoleState(t, jm, job.ID, func(j *jobs.Job) bool {
				return resends.Load() == 1 && j.Status == "phone_otp" && strings.Contains(j.SMSError, "超时")
			})
			if polls.Load() != 0 || releases.Load() != 0 {
				t.Fatal("resend re-enabled disabled automatic SMS polling")
			}
			status, body = consoleHTTPPost(t, base, "/api/jobs/"+job.ID+"/input", map[string]any{"action": "phone_otp", "value": "123456"})
			if status != http.StatusOK {
				t.Fatal(status, body)
			}
			waitConsoleState(t, jm, job.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.SMSStatus == "completed" })
			if polls.Load() != 0 || releases.Load() != 0 || completes.Load() != 1 {
				t.Fatal("manual completion did not preserve owned lease")
			}
		})
	}
}

func TestSMSZeroIntervalStillPollsAndCompletes(t *testing.T) {
	srv, jm, base := prepareSMSFlow(t, 2000)
	srv.cfg.SMSPollIntervalMS, srv.cfg.SMSPollIntervalSet = 0, true
	var polls, completes atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			fmt.Fprint(w, "ACCESS_NUMBER:zero-interval-order:12025550123")
		case "getStatus":
			polls.Add(1)
			fmt.Fprint(w, "STATUS_OK:654321")
		case "setStatus":
			if r.URL.Query().Get("status") == "6" {
				completes.Add(1)
			}
			fmt.Fprint(w, "ACCESS_READY")
		}
	}))
	defer remote.Close()
	jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
		emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
		if _, err := waitConsoleInput(ctx, jm, j.ID, "phone"); err != nil {
			return err
		}
		emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusWaitingInput})
		code, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
		if err != nil || code != "654321" {
			return errors.New("zero-interval automatic SMS missing")
		}
		emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning})
		return nil
	})
	jm.Start()
	job, err := jm.Submit(jobs.Spec{ID: "zero-interval", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	waitConsoleState(t, jm, job.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
	acquireSMSFlow(t, base, job.ID, remote.URL)
	waitConsoleState(t, jm, job.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.SMSStatus == "completed" })
	if polls.Load() != 1 || completes.Load() != 1 {
		t.Fatal("zero-interval timer duplicated or suppressed SMS", polls.Load(), completes.Load())
	}
}
