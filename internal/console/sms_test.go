package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
)

type smsTestProvider struct {
	polls, completes, releases atomic.Int32
	code                       string
	pollErr                    error
	blocking                   bool
	entered                    chan struct{}
	finished                   chan struct{}
}

func (p *smsTestProvider) GetNumber(context.Context) (providers.NumberLease, error) {
	return providers.NumberLease{ID: "lease", Number: "+12025550123", Provider: "fake"}, nil
}
func (p *smsTestProvider) GetCode(ctx context.Context, _ providers.NumberLease) (string, error) {
	if p.polls.Add(1) == 1 && p.entered != nil {
		close(p.entered)
	}
	if p.blocking {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return p.code, p.pollErr
}
func (p *smsTestProvider) Complete(context.Context, providers.NumberLease) error {
	p.completes.Add(1)
	if p.finished != nil {
		close(p.finished)
	}
	return nil
}
func (p *smsTestProvider) Release(context.Context, providers.NumberLease) error {
	p.releases.Add(1)
	return nil
}

type terminalSMSError struct{}

func (terminalSMSError) Error() string  { return "fixture API key must remain private" }
func (terminalSMSError) Terminal() bool { return true }

func TestTerminalSMSErrorStopsPollingButKeepsManualVerification(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	srv.cfg.SMSPollIntervalMS = 5
	j, _ := jm.Submit(jobs.Spec{ID: "terminal-sms", Email: "a@example.com"})
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	provider := &smsTestProvider{pollErr: terminalSMSError{}, entered: make(chan struct{}), finished: make(chan struct{})}
	lease, _ := provider.GetNumber(context.Background())
	if !srv.installSMSLease(j.ID, provider, lease) {
		t.Fatal("install failed")
	}
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("no OTP poll")
	}
	deadline := time.Now().Add(time.Second)
	for {
		current, _ := jm.Get(j.ID)
		if current.SMSStatus == "error" {
			if current.Status != "phone_otp" || strings.Contains(current.SMSError, "fixture") {
				t.Fatal("terminal provider error changed phone stage or leaked raw response")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal provider error was not reported")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if provider.polls.Load() != 1 || provider.releases.Load() != 0 {
		t.Fatal("permanent failure kept polling or released a manually usable number")
	}
	w := frontendRequest(srv, "POST", "/api/jobs/"+j.ID+"/input", map[string]any{"action": "phone_otp", "value": "654321"}, "test")
	if w.Code != http.StatusOK {
		t.Fatal("manual code rejected after terminal provider error", w.Code)
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "phone_verified", Status: jobs.StatusRunning})
	select {
	case <-provider.finished:
	case <-time.After(time.Second):
		t.Fatal("manual verification did not complete SMS lease")
	}
	if provider.completes.Load() != 1 || provider.releases.Load() != 0 {
		t.Fatal("wrong manual verification finalization")
	}
}

func TestSMSWaitsForPhoneOTPAndCompletesAtVerification(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	srv.cfg.SMSPollIntervalMS = 5
	j, err := jm.Submit(jobs.Spec{ID: "sms", Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "phone", Status: jobs.StatusWaitingInput})
	provider := &smsTestProvider{code: "123456", entered: make(chan struct{}), finished: make(chan struct{})}
	lease, _ := provider.GetNumber(context.Background())
	if !srv.installSMSLease(j.ID, provider, lease) {
		t.Fatal("install failed")
	}
	jm.UpdateSMS(j.ID, "fake", "fake", lease.Number, "number_acquired", "")
	time.Sleep(30 * time.Millisecond)
	if provider.polls.Load() != 0 {
		t.Fatal("polled before number accepted")
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("no OTP poll")
	}
	deadline := time.Now().Add(time.Second)
	for {
		input, _ := jm.InputCopy(j.ID)
		if input["phone_otp"] == "123456" {
			if input["value"] != nil || input["code"] != nil {
				t.Fatal("unscoped OTP input")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("code not submitted")
		}
		time.Sleep(time.Millisecond)
	}
	if provider.completes.Load() != 0 {
		t.Fatal("order completed before verification")
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "phone_verified", Status: jobs.StatusRunning})
	select {
	case <-provider.finished:
	case <-time.After(time.Second):
		t.Fatal("accepted phone did not complete order")
	}
	job, _ := jm.Get(j.ID)
	if job.Status == jobs.StatusSucceeded {
		t.Fatal("test must complete SMS before whole job")
	}
	if provider.releases.Load() != 0 || provider.completes.Load() != 1 {
		t.Fatal("wrong lease finalization")
	}
}
func TestSMSReleaseCancelsInflightPollExactlyOnce(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	srv.cfg.SMSPollIntervalMS = 5
	j, _ := jm.Submit(jobs.Spec{ID: "sms", Email: "a@example.com"})
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	provider := &smsTestProvider{blocking: true, entered: make(chan struct{})}
	lease, _ := provider.GetNumber(context.Background())
	if !srv.installSMSLease(j.ID, provider, lease) {
		t.Fatal("install failed")
	}
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("no poll")
	}
	done := make(chan struct{})
	go func() { srv.releaseSMS(j.ID); srv.closeSMS(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled poll leaked")
	}
	if provider.releases.Load() != 1 || provider.completes.Load() != 0 {
		t.Fatal("lease released more than once")
	}
}
func TestCustomPoolSharedAcrossJobsAndFormats(t *testing.T) {
	srv, _ := newMonitorTestServer(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"messages":[]}`)) }))
	defer remote.Close()
	entries := []string{"+12025550123----" + remote.URL, "+12025550124----" + remote.URL}
	first, err := srv.customSMSPool(strings.Join(entries, "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.customSMSPool(entries)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same pool configuration not reused")
	}
	a, err := first.GetNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.GetNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Number == b.Number {
		t.Fatal("allocated same phone to two jobs")
	}
	if _, err := first.GetNumber(context.Background()); err == nil {
		t.Fatal("used numbers repeated")
	}
}
func TestConsoleLogsRetryValidationAndDownloadIsolation(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	j, _ := jm.Submit(jobs.Spec{ID: "isolated", Email: "a@example.com"})
	jm.Cancel(j.ID)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "127.0.0.1"
		r.Header.Set("X-Console-Token", "test")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	w := request("POST", "/api/jobs/"+j.ID+"/retry", "{bad")
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	job, _ := jm.Get(j.ID)
	if job.Status != jobs.StatusCanceled {
		t.Fatal("invalid retry request scheduled job")
	}
	w = request("GET", "/api/jobs/"+j.ID+"/logs", "")
	if !strings.Contains(w.Body.String(), `"logs":"`) {
		t.Fatal("logs are not text", w.Body.String())
	}
	_ = os.WriteFile(filepath.Join(srv.cfg.OutputRoot, "sub2api-import-oauth.json"), []byte(`{"accounts":[{"email":"wrong@example.com"}]}`), 0600)
	w = request("GET", "/api/jobs/"+j.ID+"/download", "")
	if w.Code != 404 || strings.Contains(w.Body.String(), "wrong@example.com") {
		t.Fatal("download escaped job directory")
	}
}
