package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
)

type lockedSMSVault struct {
	mu     sync.Mutex
	values map[string]credentials.Credentials
}

func (v *lockedSMSVault) Save(id string, c credentials.Credentials) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.values[id] = c
	return nil
}
func (v *lockedSMSVault) Load(id string) (credentials.Credentials, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	c, ok := v.values[id]
	if !ok {
		return c, os.ErrNotExist
	}
	return c, nil
}
func (v *lockedSMSVault) Delete(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.values, id)
	return nil
}

func TestSMSOrderSecureRestartResumesAndCompletes(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	vault := &lockedSMSVault{values: map[string]credentials.Credentials{}}
	srv.monitorVault = vault
	srv.cfg.SMSPollIntervalMS = 5
	var polls, completed, released atomic.Int32
	var ready atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			_, _ = io.WriteString(w, "ACCESS_NUMBER:123:+12025550123")
		case "getStatus":
			polls.Add(1)
			if ready.Load() {
				_, _ = io.WriteString(w, "STATUS_OK:654321")
			} else {
				_, _ = io.WriteString(w, "STATUS_WAIT_CODE")
			}
		case "setStatus":
			switch r.URL.Query().Get("status") {
			case "6":
				completed.Add(1)
				_, _ = io.WriteString(w, "ACCESS_ACTIVATION")
			case "8":
				released.Add(1)
				_, _ = io.WriteString(w, "ACCESS_CANCEL")
			default:
				_, _ = io.WriteString(w, "ACCESS_READY")
			}
		}
	}))
	defer remote.Close()
	job, _ := jm.Submit(jobs.Spec{ID: "sms-restored", Email: "me@example.com"})
	jm.Emit(jobs.JobEvent{JobID: job.ID, Stage: "phone", Status: jobs.StatusWaitingInput})
	body, _ := json.Marshal(map[string]any{"providerId": "smsbower", "config": map[string]any{"apiKey": "private-api-key", "baseUrl": remote.URL, "country": "1"}})
	req := httptest.NewRequest("POST", "/api/jobs/"+job.ID+"/sms-number", strings.NewReader(string(body)))
	req.Host = "127.0.0.1"
	req.Header.Set("X-Console-Token", "test")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := vault.Load(smsCredentialID(job.ID)); err != nil {
		t.Fatal("lease must be durable before responding to acquisition", err)
	}
	jm.Emit(jobs.JobEvent{JobID: job.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	deadline := time.Now().Add(time.Second)
	for polls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if polls.Load() == 0 {
		t.Fatal("no initial SMS poll")
	}
	jm.Emit(jobs.JobEvent{JobID: job.ID, Status: "resume_available"})
	srv.closeSMS()
	if released.Load() != 0 || completed.Load() != 0 {
		t.Fatal("shutdown finalized unfinished order")
	}
	saved, err := vault.Load(smsCredentialID(job.ID))
	if err != nil {
		t.Fatal("lease not saved securely", err)
	}
	if !strings.Contains(saved.Password, "private-api-key") {
		t.Fatal("provider credentials missing from secure store")
	}
	metadata, _ := os.ReadFile(filepath.Join(srv.cfg.OutputRoot, "job-meta.json"))
	if strings.Contains(string(metadata), "private-api-key") || strings.Contains(string(metadata), "654321") {
		t.Fatal("ordinary metadata leaked SMS secrets")
	}
	resumed := &Server{cfg: srv.cfg, jobs: jm, logger: slog.Default(), monitorVault: vault}
	resumed.initializeSMS()
	t.Cleanup(resumed.closeSMS)
	oldPolls := polls.Load()
	time.Sleep(30 * time.Millisecond)
	if polls.Load() != oldPolls {
		t.Fatal("resumed order polled before phone OTP checkpoint continued")
	}
	ready.Store(true)
	jm.Emit(jobs.JobEvent{JobID: job.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	deadline = time.Now().Add(time.Second)
	for {
		values, _ := jm.InputCopy(job.ID)
		if values["phone_otp"] == "654321" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restored order did not provide code")
		}
		time.Sleep(time.Millisecond)
	}
	jm.Emit(jobs.JobEvent{JobID: job.ID, Stage: "phone_verified", Status: jobs.StatusRunning})
	deadline = time.Now().Add(time.Second)
	for completed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if completed.Load() != 1 || released.Load() != 0 {
		t.Fatal("restored order did not complete exactly once")
	}
	deadline = time.Now().Add(time.Second)
	for {
		_, err = vault.Load(smsCredentialID(job.ID))
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed lease secret not removed")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestCustomSMSPoolPositionSurvivesSecureRestart(t *testing.T) {
	srv, _ := newMonitorTestServer(t)
	vault := &lockedSMSVault{values: map[string]credentials.Credentials{}}
	srv.monitorVault = vault
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"messages":[]}`) }))
	defer remote.Close()
	lines := []string{"+12025550123----" + remote.URL, "+12025550124----" + remote.URL}
	pool, err := srv.customSMSPool(lines)
	if err != nil {
		t.Fatal(err)
	}
	first, err := pool.GetNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = pool.Complete(context.Background(), first)
	if err := srv.saveCustomSMSPool(pool); err != nil {
		t.Fatal(err)
	}
	restarted := &Server{monitorVault: vault, smsPools: map[string]*providers.CustomPool{}}
	nextPool, err := restarted.customSMSPool(lines)
	if err != nil {
		t.Fatal(err)
	}
	second, err := nextPool.GetNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Number == second.Number {
		t.Fatal("completed custom number allocated again after restart")
	}
}

type cancellableSMSVault struct {
	lockedSMSVault
	blocked atomic.Bool
}

func (v *cancellableSMSVault) SaveContext(ctx context.Context, id string, c credentials.Credentials) error {
	if v.blocked.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return v.Save(id, c)
}
func (v *cancellableSMSVault) LoadContext(ctx context.Context, id string) (credentials.Credentials, error) {
	if err := ctx.Err(); err != nil {
		return credentials.Credentials{}, err
	}
	return v.Load(id)
}
func (v *cancellableSMSVault) DeleteContext(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.Delete(id)
}
func TestSMSShutdownUsesOneDeadlineForTwentyLeases(t *testing.T) {
	srv, jm := newMonitorTestServer(t)
	vault := &cancellableSMSVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}}
	srv.monitorVault = vault
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("sms-%d", i)
		_, err := jm.Submit(jobs.Spec{ID: id, Email: id + "@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		jm.Emit(jobs.JobEvent{JobID: id, Status: "resume_available"})
		p := providers.NewSMSBowerProvider("key", "http://127.0.0.1:1", "dr", "1")
		if !srv.installSMSLease(id, p, providers.NumberLease{ID: id, Provider: "smsbower", Number: "+12025550123"}) {
			t.Fatal("install failed")
		}
	}
	vault.blocked.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = srv.CloseContext(ctx)
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatal("per-lease deadlines multiplied shutdown time", elapsed)
	}
	done := make(chan struct{})
	go func() { srv.smsWG.Wait(); close(done) }()
	// The shared shutdown deadline above still limits credential operations
	// and CloseContext to one budget. Workers then persist interruption metadata
	// sequentially (including fsync), which can take longer under a race run's
	// concurrent disk load. Give that local cleanup its own bounded grace period.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SMS workers did not observe shared deadline")
	}
}
