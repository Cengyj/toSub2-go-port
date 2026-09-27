package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func waitConsoleState(t *testing.T, jm *jobs.Manager, id string, predicate func(*jobs.Job) bool) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j, exists := jm.Get(id)
		if exists && predicate(j) {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	j, _ := jm.Get(id)
	t.Fatalf("task did not reach expected state: status=%s stage=%s smsStatus=%s error=%s", j.Status, j.Stage, j.SMSStatus, j.SMSError)
	return nil
}

func waitConsoleInput(ctx context.Context, jm *jobs.Manager, id, key string) (string, error) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if value, ok := jm.ConsumeInput(id, key); ok {
			return value, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func consoleHTTPPost(t *testing.T, base, path string, payload any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	req.Header.Set("X-Console-Token", "test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, result
}

func prepareSMSFlow(t *testing.T, timeoutMS int) (*Server, *jobs.Manager, string) {
	t.Helper()
	srv, jm := newMonitorTestServer(t)
	srv.monitorVault = &lockedSMSVault{values: map[string]credentials.Credentials{}}
	srv.cfg.SMSPollIntervalMS, srv.cfg.SMSPollTimeoutMS = 3, timeoutMS
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := jm.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return srv, jm, httpServer.URL
}

func acquireSMSFlow(t *testing.T, base, id, remote string) {
	t.Helper()
	status, body := consoleHTTPPost(t, base, "/api/jobs/"+id+"/sms-number", map[string]any{"providerId": "smsbower", "config": map[string]any{"apiKey": "fixture-private-key", "baseUrl": remote, "country": "1", "maxPrice": "0.25"}})
	if status != http.StatusOK {
		t.Fatalf("number request rejected: %d %v", status, body)
	}
}

func TestSMSBowerConsoleManagerFlowReadinessAfterAcceptance(t *testing.T) {
	_, jm, base := prepareSMSFlow(t, 2000)
	var accepted atomic.Bool
	var readies, earlyReadies, completes, releases atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch q.Get("action") {
		case "getNumber":
			if q.Get("maxPrice") != "0.25" {
				t.Error("maximum price was lost")
			}
			fmt.Fprint(w, "ACCESS_NUMBER:flow-order:12025550123")
		case "setStatus":
			switch q.Get("status") {
			case "1":
				readies.Add(1)
				if !accepted.Load() {
					earlyReadies.Add(1)
				}
				// Advisory readiness failure must not destroy an accepted number.
				w.WriteHeader(http.StatusServiceUnavailable)
			case "6":
				completes.Add(1)
				fmt.Fprint(w, "ACCESS_ACTIVATION")
			case "8":
				releases.Add(1)
				fmt.Fprint(w, "ACCESS_CANCEL")
			}
		case "getStatus":
			fmt.Fprint(w, "STATUS_OK:654321")
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer remote.Close()
	acceptNumber := make(chan struct{})
	jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
		emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
		number, err := waitConsoleInput(ctx, jm, j.ID, "phone")
		if err != nil || number != "+12025550123" {
			return errors.New("phone submission missing")
		}
		select {
		case <-acceptNumber:
		case <-ctx.Done():
			return ctx.Err()
		}
		accepted.Store(true)
		emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusWaitingInput})
		code, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
		if err != nil || code != "654321" {
			return errors.New("SMS submission missing")
		}
		emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning})
		return nil
	})
	jm.Start()
	j, _ := jm.Submit(jobs.Spec{ID: "sms-full-chain", Email: "owner@example.test"})
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
	acquireSMSFlow(t, base, j.ID, remote.URL)
	if readies.Load() != 0 {
		t.Fatal("provider marked ready before phone acceptance")
	}
	close(acceptNumber)
	finished := waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.SMSStatus == "completed" })
	if finished.SMSProviderName != "SMSBower" || readies.Load() != 1 || earlyReadies.Load() != 0 || completes.Load() != 1 || releases.Load() != 0 {
		t.Fatal("readiness, display name or order completion incompatible")
	}
}

func TestSMSConsoleTimeoutResendDuplicateThenManualComplete(t *testing.T) {
	_, jm, base := prepareSMSFlow(t, 400)
	var available atomic.Bool
	var polls, readies, releases, completes atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			fmt.Fprint(w, "ACCESS_NUMBER:timeout-order:12025550123")
		case "getStatus":
			polls.Add(1)
			if available.Load() {
				fmt.Fprint(w, "STATUS_OK:654321")
			} else {
				fmt.Fprint(w, "STATUS_WAIT_CODE")
			}
		case "setStatus":
			switch r.URL.Query().Get("status") {
			case "1":
				readies.Add(1)
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
		for _, want := range []string{"r", "654321", "123456"} {
			emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusWaitingInput})
			value, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
			if err != nil || value != want {
				return errors.New("unexpected code or resend order")
			}
		}
		emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning})
		return nil
	})
	jm.Start()
	j, _ := jm.Submit(jobs.Spec{ID: "sms-resume", Email: "owner@example.test"})
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
	acquireSMSFlow(t, base, j.ID, remote.URL)
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return strings.Contains(j.SMSError, "超时") })
	if releases.Load() != 0 {
		t.Fatal("poll timeout released manually usable number")
	}
	available.Store(true)
	status, response := consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/input", map[string]any{"action": "resend_phone"})
	if status != http.StatusOK {
		t.Fatal(status, response)
	}
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return strings.Contains(j.SMSError, "已提交过") })
	lastPoll := polls.Load()
	time.Sleep(20 * time.Millisecond)
	if polls.Load() != lastPoll || readies.Load() != 2 || releases.Load() != 0 {
		t.Fatal("resend did not restart readiness or duplicate OTP kept polling")
	}
	status, response = consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/input", map[string]any{"action": "phone_otp", "value": "123456"})
	if status != http.StatusOK {
		t.Fatal(status, response)
	}
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.SMSStatus == "completed" })
	if completes.Load() != 1 || releases.Load() != 0 {
		t.Fatal("manual input did not complete retained lease")
	}
}

func TestSMSProviderHTTP429PreservesManualChangeAndReleasesOnce(t *testing.T) {
	_, jm, base := prepareSMSFlow(t, 2000)
	var polls, releases atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			fmt.Fprint(w, "ACCESS_NUMBER:limited-order:12025550123")
		case "getStatus":
			polls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "fixture-private-key must never appear in console")
		case "setStatus":
			if r.URL.Query().Get("status") == "8" {
				releases.Add(1)
			}
			fmt.Fprint(w, "ACCESS_READY")
		}
	}))
	defer remote.Close()
	jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
		for _, step := range []struct{ stage, key, value string }{
			{"phone", "phone", "+12025550123"},
			{"sms_otp", "phone_otp", "p"},
			{"phone", "phone", "+12025550124"},
			{"sms_otp", "phone_otp", "123456"},
		} {
			emit(jobs.JobEvent{Stage: step.stage, Status: jobs.StatusWaitingInput})
			value, err := waitConsoleInput(ctx, jm, j.ID, step.key)
			if err != nil || value != step.value {
				return errors.New("unexpected manual fallback sequence")
			}
		}
		emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning})
		return nil
	})
	jm.Start()
	j, _ := jm.Submit(jobs.Spec{ID: "sms-http429", Email: "owner@example.test"})
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
	acquireSMSFlow(t, base, j.ID, remote.URL)
	failed := waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.SMSStatus == "error" })
	if strings.Contains(failed.SMSError, "fixture-private-key") || releases.Load() != 0 {
		t.Fatal("provider response leaked or terminal polling discarded the number")
	}
	time.Sleep(20 * time.Millisecond)
	if polls.Load() != 1 {
		t.Fatal("HTTP 429 provider failure continued automatic polling")
	}
	status, response := consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/input", map[string]any{"action": "change_phone"})
	if status != http.StatusOK {
		t.Fatal(status, response)
	}
	changed := waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
	if releases.Load() != 1 || changed.CurrentPhone != "" || changed.SMSProviderID != "" || changed.SMSStatus != "idle" {
		t.Fatal("phone replacement retained provider state or failed to release")
	}
	status, response = consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/input", map[string]any{"action": "phone", "value": "+12025550124"})
	if status != http.StatusOK {
		t.Fatal(status, response)
	}
	manual := waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone_otp" })
	if manual.CurrentPhone != "+12025550124" {
		t.Fatal("manual phone did not reach visible job state")
	}
	status, response = consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/input", map[string]any{"action": "phone_otp", "value": "123456"})
	if status != http.StatusOK {
		t.Fatal(status, response)
	}
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded })
	if releases.Load() != 1 {
		t.Fatal("completed manual fallback released original number twice")
	}
}
