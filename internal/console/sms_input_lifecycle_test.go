package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

type smsAdmissionCounts struct {
	numbers, ready, polls, completed, released atomic.Int32
}

func newSMSAdmissionProvider(t *testing.T) (*httptest.Server, *smsAdmissionCounts) {
	t.Helper()
	counts := &smsAdmissionCounts{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			counts.numbers.Add(1)
			fmt.Fprint(w, "ACCESS_NUMBER:admission-order:12025550123")
		case "getStatus":
			counts.polls.Add(1)
			fmt.Fprint(w, "STATUS_OK:654321")
		case "setStatus":
			switch r.URL.Query().Get("status") {
			case "1":
				counts.ready.Add(1)
			case "6":
				counts.completed.Add(1)
			case "8":
				counts.released.Add(1)
			}
			fmt.Fprint(w, "ACCESS_READY")
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	return remote, counts
}

func waitSMSAdmissionSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("SMS lifecycle did not reach its synchronization point")
	}
}

func waitSMSAdmissionGate(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSMSFlowConsumedEventsKeepLeaseAndDoNotResubmit(t *testing.T) {
	for _, prompt := range []struct {
		name, phoneStatus, smsStage, smsStatus string
	}{
		{"protocol", "awaiting_input", "sms_otp", "awaiting_input"},
		{"waiting", jobs.StatusWaitingInput, "phone_otp", jobs.StatusWaitingInput},
		{"legacy", "phone", "sms_otp", "phone_otp"},
	} {
		t.Run(prompt.name, func(t *testing.T) {
			srv, jm, base := prepareSMSFlow(t, 2000)
			remote, counts := newSMSAdmissionProvider(t)
			defer remote.Close()
			phoneConsumed, smsConsumed := make(chan struct{}), make(chan struct{})
			acceptPhone, acceptCode := make(chan struct{}), make(chan struct{})
			var receipts atomic.Int32
			jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
				emit(jobs.JobEvent{Stage: "phone", Status: prompt.phoneStatus})
				number, err := waitConsoleInput(ctx, jm, j.ID, "phone")
				if err != nil || number != "+12025550123" {
					return errors.New("automatic phone was not delivered")
				}
				// Flow.ask emits the same stage with running after consuming input.
				emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusRunning})
				close(phoneConsumed)
				if err := waitSMSAdmissionGate(ctx, acceptPhone); err != nil {
					return err
				}
				emit(jobs.JobEvent{Stage: prompt.smsStage, Status: prompt.smsStatus})
				code, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
				if err != nil || code != "654321" {
					return errors.New("automatic SMS code was not delivered")
				}
				receipts.Add(1)
				emit(jobs.JobEvent{Stage: prompt.smsStage, Status: jobs.StatusRunning})
				close(smsConsumed)
				if err := waitSMSAdmissionGate(ctx, acceptCode); err != nil {
					return err
				}
				emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning})
				return nil
			})
			jm.Start()
			j, err := jm.Submit(jobs.Spec{ID: "consumed-events", Email: "owner@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
			acquireSMSFlow(t, base, j.ID, remote.URL)
			waitSMSAdmissionSignal(t, phoneConsumed)
			time.Sleep(25 * time.Millisecond)
			srv.smsMu.Lock()
			installed := srv.smsLeases[j.ID] != nil
			srv.smsMu.Unlock()
			if !installed || counts.released.Load() != 0 || counts.polls.Load() != 0 || counts.ready.Load() != 0 {
				t.Fatal("phone consumed event released the lease or polled before acceptance")
			}
			close(acceptPhone)
			waitSMSAdmissionSignal(t, smsConsumed)
			time.Sleep(25 * time.Millisecond)
			srv.smsMu.Lock()
			lease := srv.smsLeases[j.ID]
			retained := lease != nil && lease.submitted && lease.pendingCode == "654321" && lease.seen["654321"] && !lease.pollStopped
			srv.smsMu.Unlock()
			input, _ := jm.InputCopy(j.ID)
			job, _ := jm.Get(j.ID)
			if !retained || input["phone_otp"] != nil || job.Status != "working" || counts.polls.Load() != 1 || counts.ready.Load() != 1 || counts.completed.Load() != 0 || counts.released.Load() != 0 {
				t.Fatal("SMS consumed event reset submission, repeated polling, or finalized early")
			}
			close(acceptCode)
			waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.SMSStatus == "completed" })
			if receipts.Load() != 1 || counts.numbers.Load() != 1 || counts.polls.Load() != 1 || counts.ready.Load() != 1 || counts.completed.Load() != 1 || counts.released.Load() != 0 {
				t.Fatal("provider lifecycle was not exactly once")
			}
		})
	}
}

type smsAdmissionBlockingVault struct {
	lockedSMSVault
	block   func(privateSMSState) bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (v *smsAdmissionBlockingVault) Save(id string, value credentials.Credentials) error {
	var state privateSMSState
	if strings.HasPrefix(id, "sms-job-") && json.Unmarshal([]byte(value.Password), &state) == nil && v.block(state) {
		v.once.Do(func() { close(v.entered); <-v.release })
	}
	return v.lockedSMSVault.Save(id, value)
}

func newSMSAdmissionBlockingVault(t *testing.T, predicate func(privateSMSState) bool) (*smsAdmissionBlockingVault, func()) {
	t.Helper()
	v := &smsAdmissionBlockingVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}, block: predicate, entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(v.release) }) }
	t.Cleanup(unblock)
	return v, unblock
}

func TestSMSNumberStageRaceReleasesUnsubmittedLease(t *testing.T) {
	srv, jm, base := prepareSMSFlow(t, 2000)
	remote, counts := newSMSAdmissionProvider(t)
	defer remote.Close()
	vault, unblock := newSMSAdmissionBlockingVault(t, func(state privateSMSState) bool { return state.Action == "" })
	srv.monitorVault = vault
	consumed := make(chan struct{})
	jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
		emit(jobs.JobEvent{Stage: "phone", Status: "awaiting_input"})
		number, err := waitConsoleInput(ctx, jm, j.ID, "phone")
		if err != nil || number != "+12025550999" {
			return errors.New("automatic number replaced the admitted manual phone")
		}
		emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusRunning})
		close(consumed)
		<-ctx.Done()
		return ctx.Err()
	})
	jm.Start()
	j, err := jm.Submit(jobs.Spec{ID: "phone-admission-race", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
	type response struct {
		status int
		err    error
	}
	result := make(chan response, 1)
	body, _ := json.Marshal(map[string]any{"providerId": "smsbower", "config": map[string]any{"apiKey": "fixture-private-key", "baseUrl": remote.URL, "country": "1"}})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/jobs/"+j.ID+"/sms-number", strings.NewReader(string(body)))
		if err != nil {
			result <- response{err: err}
			return
		}
		req.Header.Set("X-Console-Token", "test")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			result <- response{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		result <- response{status: resp.StatusCode}
	}()
	waitSMSAdmissionSignal(t, vault.entered)
	if !jm.InputForStage(j.ID, "phone", map[string]any{"phone": "+12025550999"}) {
		t.Fatal("manual phone was not admitted during provider persistence")
	}
	waitSMSAdmissionSignal(t, consumed)
	unblock()
	select {
	case reply := <-result:
		if reply.err != nil || reply.status != http.StatusConflict {
			t.Fatalf("stale automatic acquisition accepted: status=%d err=%v", reply.status, reply.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stale acquisition did not terminate")
	}
	srv.smsMu.Lock()
	remaining := len(srv.smsLeases)
	srv.smsMu.Unlock()
	current, _ := jm.Get(j.ID)
	input, _ := jm.InputCopy(j.ID)
	if remaining != 0 || counts.released.Load() != 1 || counts.polls.Load() != 0 || counts.ready.Load() != 0 || counts.completed.Load() != 0 || current.CurrentPhone != "+12025550999" || input["phone"] != nil {
		t.Fatal("rejected automatic input leaked its order or replaced the manual phone")
	}
}

func TestSMSCodeStageRaceDoesNotOverwriteAdmittedManualCode(t *testing.T) {
	srv, jm, base := prepareSMSFlow(t, 2000)
	remote, counts := newSMSAdmissionProvider(t)
	defer remote.Close()
	vault, unblock := newSMSAdmissionBlockingVault(t, func(state privateSMSState) bool { return state.PendingCode == "654321" && state.Action == "" })
	srv.monitorVault = vault
	consumed, finish := make(chan struct{}), make(chan struct{})
	jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
		emit(jobs.JobEvent{Stage: "phone", Status: "awaiting_input"})
		if _, err := waitConsoleInput(ctx, jm, j.ID, "phone"); err != nil {
			return err
		}
		emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusRunning})
		emit(jobs.JobEvent{Stage: "sms_otp", Status: "awaiting_input"})
		value, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
		if err != nil || value != "123456" {
			return errors.New("automatic code replaced the admitted manual code")
		}
		emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusRunning})
		close(consumed)
		if err := waitSMSAdmissionGate(ctx, finish); err != nil {
			return err
		}
		emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning})
		return nil
	})
	jm.Start()
	j, err := jm.Submit(jobs.Spec{ID: "sms-admission-race", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
	acquireSMSFlow(t, base, j.ID, remote.URL)
	waitSMSAdmissionSignal(t, vault.entered)
	if !jm.InputForStage(j.ID, "phone_otp", map[string]any{"phone_otp": "123456"}) {
		t.Fatal("manual code was not admitted during provider persistence")
	}
	waitSMSAdmissionSignal(t, consumed)
	unblock()
	deadline := time.Now().Add(time.Second)
	for {
		saved, err := vault.Load(smsCredentialID(j.ID))
		var state privateSMSState
		if err == nil && json.Unmarshal([]byte(saved.Password), &state) == nil && state.PendingCode == "" && !state.Seen["654321"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rejected automatic code remained in private replay state")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)
	input, _ := jm.InputCopy(j.ID)
	if input["phone_otp"] != nil || counts.polls.Load() != 1 || counts.ready.Load() != 1 || counts.released.Load() != 0 || counts.completed.Load() != 0 {
		t.Fatal("automatic input overwrote manual input or repeated provider work")
	}
	close(finish)
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.SMSStatus == "completed" })
	if counts.completed.Load() != 1 || counts.released.Load() != 0 || counts.polls.Load() != 1 {
		t.Fatal("manual winner did not complete the retained lease exactly once")
	}
}
