package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
	"tosub2-go-port/internal/store"
)

func TestSMSDeferredPhoneSendRetainsLeaseForExplicitRecovery(t *testing.T) {
	for _, action := range []string{"retry-same", "cancel", "different-number"} {
		t.Run(action, func(t *testing.T) {
			srv, jm, base := prepareSMSFlow(t, 2000)
			remote, counts := newSMSAdmissionProvider(t)
			defer remote.Close()
			paused, manual := make(chan struct{}), make(chan struct{})
			accept := make(chan struct{})
			var sends, validations atomic.Int32
			jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
				emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
				phone, err := waitConsoleInput(ctx, jm, j.ID, "phone")
				if err != nil || phone != "+12025550123" {
					return errors.New("initial leased phone missing")
				}
				sends.Add(1)
				emit(jobs.JobEvent{Stage: "phone_send_deferred", Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": phone, "phoneRetryRequired": true, "phoneError": "Temporary phone service error"}})
				emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
				close(paused)
				phone, err = waitConsoleInput(ctx, jm, j.ID, "phone")
				if err != nil {
					return err
				}
				sends.Add(1)
				close(manual)
				if err := waitSMSAdmissionGate(ctx, accept); err != nil {
					return err
				}
				emit(jobs.JobEvent{Stage: "phone_sent", Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": phone, "phoneRetryRequired": false}})
				emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusWaitingInput})
				code, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
				if err != nil || code != "654321" {
					return errors.New("retry did not preserve original SMS lease")
				}
				validations.Add(1)
				emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": phone, "phoneRetryRequired": false}})
				return nil
			})
			jm.Start()
			j, err := jm.Submit(jobs.Spec{ID: "send-deferred", Email: "owner@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
			acquireSMSFlow(t, base, j.ID, remote.URL)
			waitSMSAdmissionSignal(t, paused)
			time.Sleep(25 * time.Millisecond)
			srv.smsMu.Lock()
			lease := srv.smsLeases[j.ID]
			srv.smsMu.Unlock()
			current, _ := jm.Get(j.ID)
			if lease == nil || counts.released.Load() != 0 || counts.numbers.Load() != 1 || counts.polls.Load() != 0 || sends.Load() != 1 || validations.Load() != 0 {
				t.Fatal("deferred phone send released/replaced its lease or retried without manual input")
			}
			if current.CurrentPhone != "+12025550123" || srv.publicJob(current)["phoneRetryRequired"] != true {
				t.Fatal("deferred phone state lost its number or public pause marker")
			}
			if action == "cancel" {
				if status, _ := consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/cancel", map[string]any{}); status != http.StatusOK {
					t.Fatal("deferred job cancellation failed")
				}
				if counts.released.Load() != 1 || counts.numbers.Load() != 1 || sends.Load() != 1 {
					t.Fatal("cancellation did not release exactly the retained lease")
				}
				return
			}
			number := "+12025550123"
			if action == "different-number" {
				number = "+12025550999"
			}
			if status, _ := consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/input", map[string]any{"action": "phone", "value": number}); status != http.StatusOK {
				t.Fatal("manual phone retry was rejected")
			}
			waitSMSAdmissionSignal(t, manual)
			if action == "different-number" {
				if counts.released.Load() != 1 || counts.polls.Load() != 0 || counts.numbers.Load() != 1 || sends.Load() != 2 {
					t.Fatal("explicit different phone did not release only the old lease")
				}
				return
			}
			time.Sleep(15 * time.Millisecond)
			if counts.released.Load() != 0 || counts.polls.Load() != 0 || counts.ready.Load() != 0 || counts.numbers.Load() != 1 || sends.Load() != 2 {
				t.Fatal("same-number manual retry discarded lease or polled before phone acceptance")
			}
			close(accept)
			waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.SMSStatus == "completed" })
			if counts.released.Load() != 0 || counts.completed.Load() != 1 || counts.polls.Load() != 1 || counts.numbers.Load() != 1 || sends.Load() != 2 || validations.Load() != 1 {
				t.Fatal("same-number recovery did not use the original lease exactly once")
			}
		})
	}
}

func newSMSDeferredProvider(t *testing.T) (*httptest.Server, *smsAdmissionCounts, *atomic.Value) {
	t.Helper()
	counts := &smsAdmissionCounts{}
	code := &atomic.Value{}
	code.Store("654321")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			n := counts.numbers.Add(1)
			fmt.Fprintf(w, "ACCESS_NUMBER:deferred-order-%d:120255501%02d", n, 22+n)
		case "getStatus":
			counts.polls.Add(1)
			fmt.Fprint(w, "STATUS_OK:"+code.Load().(string))
		case "setStatus":
			switch r.URL.Query().Get("status") {
			case "1":
				counts.ready.Add(1)
			case "6":
				counts.completed.Add(1)
			case "8":
				counts.released.Add(1)
			}
			fmt.Fprint(w, map[string]string{"1": "ACCESS_READY", "6": "ACCESS_ACTIVATION", "8": "ACCESS_CANCEL"}[r.URL.Query().Get("status")])
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	return remote, counts, code
}

func TestSMSDeferredOTPAndResendRequireManualRecovery(t *testing.T) {
	for _, deferredStage := range []string{"sms_otp_deferred", "sms_resend_deferred"} {
		for _, action := range []string{"manual-code", "manual-resend", "change", "cancel"} {
			t.Run(deferredStage+"/"+action, func(t *testing.T) {
				srv, jm, base := prepareSMSFlow(t, 2000)
				remote, counts, providerCode := newSMSDeferredProvider(t)
				defer remote.Close()
				paused, manual, accept := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var validations, resends atomic.Int32
				jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
					emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
					phone, err := waitConsoleInput(ctx, jm, j.ID, "phone")
					if err != nil {
						return err
					}
					emit(jobs.JobEvent{Stage: "phone_sent", Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": phone, "phoneRetryRequired": false}})
					if deferredStage == "sms_otp_deferred" {
						emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusWaitingInput})
						code, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
						if err != nil || code != "654321" {
							return errors.New("initial automatic SMS missing")
						}
						validations.Add(1)
					} else {
						// The protocol owner separately exercises the HTTP resend.
						// This fixture starts at its resulting structured failure.
						resends.Add(1)
					}
					emit(jobs.JobEvent{Stage: deferredStage, Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": phone, "phoneRetryRequired": true}})
					emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusWaitingInput})
					close(paused)
					value, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
					if err != nil {
						return err
					}
					if value == "p" {
						emit(jobs.JobEvent{Stage: "phone_changed", Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": "", "phoneRetryRequired": false}})
						emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
						close(manual)
						<-ctx.Done()
						return ctx.Err()
					}
					if value == "r" {
						resends.Add(1)
					} else if value == "112233" {
						validations.Add(1)
					} else {
						return errors.New("deferred flow replayed an old SMS code")
					}
					close(manual)
					if err := waitSMSAdmissionGate(ctx, accept); err != nil {
						return err
					}
					if value == "r" {
						emit(jobs.JobEvent{Stage: "sms_resent", Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": phone, "phoneRetryRequired": false}})
						emit(jobs.JobEvent{Stage: "sms_otp", Status: jobs.StatusWaitingInput})
						code, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp")
						if err != nil || code != "112233" {
							return errors.New("successful manual resend did not use a fresh code")
						}
						validations.Add(1)
					}
					emit(jobs.JobEvent{Stage: "phone_verified", Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": phone, "phoneRetryRequired": false}})
					return nil
				})
				jm.Start()
				j, err := jm.Submit(jobs.Spec{ID: "otp-deferred", Email: "owner@example.test"})
				if err != nil {
					t.Fatal(err)
				}
				waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
				acquireSMSFlow(t, base, j.ID, remote.URL)
				waitSMSAdmissionSignal(t, paused)
				time.Sleep(20 * time.Millisecond)
				oldPolls, oldValidations, oldResends := counts.polls.Load(), validations.Load(), resends.Load()
				time.Sleep(25 * time.Millisecond)
				srv.smsMu.Lock()
				lease := srv.smsLeases[j.ID]
				retained := lease != nil && lease.pendingCode == ""
				srv.smsMu.Unlock()
				current, _ := jm.Get(j.ID)
				if !retained || !current.PhoneRetryRequired || current.CurrentPhone != "+12025550123" || counts.released.Load() != 0 || counts.completed.Load() != 0 || counts.numbers.Load() != 1 || counts.polls.Load() != oldPolls || validations.Load() != oldValidations || resends.Load() != oldResends {
					t.Fatal("deferred SMS did not retain a quiescent lease without replay state")
				}
				if action == "cancel" {
					if status, _ := consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/cancel", map[string]any{}); status != http.StatusOK || counts.released.Load() != 1 {
						t.Fatal("deferred SMS cancellation did not release its lease")
					}
					return
				}
				body := map[string]any{"action": "phone_otp", "value": "112233"}
				if action == "change" {
					body = map[string]any{"action": "change_phone"}
				} else if action == "manual-resend" {
					body = map[string]any{"action": "resend_phone"}
					providerCode.Store("112233")
				}
				if status, _ := consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/input", body); status != http.StatusOK {
					t.Fatal("explicit SMS recovery input was rejected")
				}
				waitSMSAdmissionSignal(t, manual)
				if action == "change" {
					if counts.released.Load() != 1 || counts.numbers.Load() != 1 || counts.polls.Load() != oldPolls {
						t.Fatal("explicit change did not release exactly the original order")
					}
					return
				}
				time.Sleep(15 * time.Millisecond)
				if counts.polls.Load() != oldPolls || counts.released.Load() != 0 || counts.completed.Load() != 0 {
					t.Fatal("manual retry restarted polling before protocol acceptance")
				}
				close(accept)
				waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == jobs.StatusSucceeded && j.SMSStatus == "completed" })
				if counts.numbers.Load() != 1 || counts.completed.Load() != 1 || counts.released.Load() != 0 || validations.Load() != oldValidations+1 {
					t.Fatal("manual recovery did not complete the original order exactly once")
				}
			})
		}
	}
}

func TestSMSDeferredPhoneAllowsExplicitProviderReplacement(t *testing.T) {
	_, jm, base := prepareSMSFlow(t, 2000)
	remote, counts, _ := newSMSDeferredProvider(t)
	defer remote.Close()
	paused, replaced := make(chan struct{}), make(chan struct{})
	jm.SetRunner(func(ctx context.Context, j *jobs.Job, emit func(jobs.JobEvent)) error {
		emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
		phone, err := waitConsoleInput(ctx, jm, j.ID, "phone")
		if err != nil || phone != "+12025550123" {
			return errors.New("initial phone missing")
		}
		emit(jobs.JobEvent{Stage: "phone_send_deferred", Status: jobs.StatusRunning, Data: map[string]any{"currentPhone": phone, "phoneRetryRequired": true}})
		emit(jobs.JobEvent{Stage: "phone", Status: jobs.StatusWaitingInput})
		close(paused)
		phone, err = waitConsoleInput(ctx, jm, j.ID, "phone")
		if err != nil || phone != "+12025550124" {
			return errors.New("explicitly replaced phone missing")
		}
		close(replaced)
		<-ctx.Done()
		return ctx.Err()
	})
	jm.Start()
	j, err := jm.Submit(jobs.Spec{ID: "provider-replace", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.Status == "phone" })
	acquireSMSFlow(t, base, j.ID, remote.URL)
	waitSMSAdmissionSignal(t, paused)
	time.Sleep(25 * time.Millisecond)
	if counts.numbers.Load() != 1 || counts.released.Load() != 0 {
		t.Fatal("deferred lease was replaced before an explicit HTTP request")
	}
	acquireSMSFlow(t, base, j.ID, remote.URL)
	waitSMSAdmissionSignal(t, replaced)
	if counts.numbers.Load() != 2 || counts.released.Load() != 1 || counts.polls.Load() != 0 {
		t.Fatal("explicit provider replacement did not acquire once and release the old lease once")
	}
}

func TestSMSDeferredSecureRestartNeverReplaysPendingOTP(t *testing.T) {
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	const id, email, phone = "deferred-restart", "owner@example.test", "+12025550123"
	remote, counts, _ := newSMSDeferredProvider(t)
	defer remote.Close()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"id": id, "email": email, "status": "phone_otp", "current_phone": phone, "sms_provider_id": "smsbower", "sms_provider_name": "SMSBower", "sms_status": "submitted", "phone_retry_required": true}
	if err := st.WriteJSON(context.Background(), "job-meta.json", map[string]any{"jobs": []any{record}}); err != nil {
		t.Fatal(err)
	}
	state := privateSMSState{Version: 1, JobID: id, Email: email, Provider: "smsbower", APIKey: "fixture-private-key", Base: remote.URL, Country: "1", Service: "dr", Lease: providers.NumberLease{ID: "retained-order", Provider: "smsbower", Number: phone}}
	data, _ := json.Marshal(state)
	var legacy map[string]any
	_ = json.Unmarshal(data, &legacy)
	legacy["pendingCode"], legacy["seen"] = "654321", map[string]bool{"654321": true}
	data, _ = json.Marshal(legacy)
	vault := &lockedSMSVault{values: map[string]credentials.Credentials{smsCredentialID(id): {Password: string(data)}}}
	srv, jm, base := newPersistedSMSRestoreServer(t, st, vault, slog.New(slog.NewTextHandler(io.Discard, nil)))
	public, body := smsRestorePublicJobs(t, base)
	if public[id]["status"] == "resume_available" || public[id]["smsStatus"] != "interrupted" || strings.Contains(body, "654321") || strings.Contains(body, "fixture-private-key") {
		t.Fatalf("restart state status=%v sms=%v", public[id]["status"], public[id]["smsStatus"])
	}
	jm.Emit(jobs.JobEvent{JobID: id, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	time.Sleep(35 * time.Millisecond)
	srv.smsMu.Lock()
	installed := len(srv.smsLeases)
	srv.smsMu.Unlock()
	if installed != 0 || counts.numbers.Load() != 0 || counts.polls.Load() != 0 || counts.ready.Load() != 0 || counts.released.Load() != 0 || counts.completed.Load() != 0 {
		t.Fatal("restart continued historical SMS authentication")
	}
	if _, exists := jm.ConsumeInput(id, "phone", "phone_otp", "sms_otp", "code", "value"); exists {
		t.Fatal("restart republished private phone/code input")
	}
	preserved, err := vault.Load(smsCredentialID(id))
	if err != nil || strings.Contains(preserved.Password, "654321") || strings.Contains(preserved.Password, "pendingCode") || strings.Contains(preserved.Password, `"seen"`) || !strings.Contains(preserved.Password, "fixture-private-key") {
		t.Fatal("legacy OTP not cleared or provider credentials lost")
	}
}

func TestSMSDeferredBeforePersistedCodeAdmissionCannotReplay(t *testing.T) {
	srv, jm, base := prepareSMSFlow(t, 2000)
	remote, counts, providerCode := newSMSDeferredProvider(t)
	defer remote.Close()
	vault, unblock := newSMSAdmissionBlockingVault(t, func(state privateSMSState) bool { return counts.polls.Load() > 0 && state.Action == "" })
	srv.monitorVault = vault
	j, err := jm.Submit(jobs.Spec{ID: "deferred-persistence-race", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "phone", Status: jobs.StatusWaitingInput})
	acquireSMSFlow(t, base, j.ID, remote.URL)
	if _, exists := jm.ConsumeInput(j.ID, "phone"); !exists {
		t.Fatal("fixture did not receive provider number")
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "phone_sent", Status: jobs.StatusRunning, Data: map[string]any{"phoneRetryRequired": false}})
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	waitSMSAdmissionSignal(t, vault.entered)
	// The lease goroutine cannot consume either event until persistence ends.
	// Its already-polled code must still fail the atomic manager admission.
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp_deferred", Status: jobs.StatusRunning, Data: map[string]any{"phoneRetryRequired": true}})
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	unblock()
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.PhoneRetryRequired && j.SMSStatus == "error" })
	time.Sleep(20 * time.Millisecond)
	if _, exists := jm.ConsumeInput(j.ID, "phone_otp", "sms_otp", "code", "value"); exists {
		t.Fatal("late subscriber admitted a polled code after protocol deferral")
	}
	srv.smsMu.Lock()
	lease := srv.smsLeases[j.ID]
	safe := lease != nil && lease.retryPaused && lease.pendingCode == "" && lease.seen["654321"]
	srv.smsMu.Unlock()
	if !safe || counts.polls.Load() != 1 || counts.numbers.Load() != 1 || counts.released.Load() != 0 {
		t.Fatal("late subscriber lost the lease, forgot code history, or continued polling")
	}
	providerCode.Store("112233")
	if status, _ := consoleHTTPPost(t, base, "/api/jobs/"+j.ID+"/input", map[string]any{"action": "resend_phone"}); status != http.StatusOK {
		t.Fatal("late subscriber recovery rejected explicit resend")
	}
	if value, exists := jm.ConsumeInput(j.ID, "phone_otp"); !exists || value != "r" {
		t.Fatal("manual recovery resend was not delivered")
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_resent", Status: jobs.StatusRunning, Data: map[string]any{"phoneRetryRequired": false}})
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if code, err := waitConsoleInput(ctx, jm, j.ID, "phone_otp"); err != nil || code != "112233" {
		t.Fatal("late subscriber recovery replayed old code instead of fresh SMS")
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "phone_verified", Status: jobs.StatusRunning})
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.SMSStatus == "completed" })
	if counts.polls.Load() != 2 || counts.numbers.Load() != 1 || counts.released.Load() != 0 || counts.completed.Load() != 1 {
		t.Fatal("late subscriber recovery duplicated its provider lifecycle")
	}
}

func TestSMSDeferredWhileProviderPollIsInFlightDropsLateCode(t *testing.T) {
	srv, jm, base := prepareSMSFlow(t, 2000)
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	counts := &smsAdmissionCounts{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			counts.numbers.Add(1)
			fmt.Fprint(w, "ACCESS_NUMBER:inflight-order:12025550123")
		case "getStatus":
			counts.polls.Add(1)
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
				fmt.Fprint(w, "STATUS_OK:654321")
			case <-r.Context().Done():
			}
		case "setStatus":
			switch r.URL.Query().Get("status") {
			case "1":
				counts.ready.Add(1)
			case "6":
				counts.completed.Add(1)
			case "8":
				counts.released.Add(1)
			}
			fmt.Fprint(w, map[string]string{"1": "ACCESS_READY", "6": "ACCESS_ACTIVATION", "8": "ACCESS_CANCEL"}[r.URL.Query().Get("status")])
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer remote.Close()
	j, err := jm.Submit(jobs.Spec{ID: "deferred-inflight-poll", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "phone", Status: jobs.StatusWaitingInput})
	acquireSMSFlow(t, base, j.ID, remote.URL)
	if _, exists := jm.ConsumeInput(j.ID, "phone"); !exists {
		t.Fatal("fixture did not receive provider number")
	}
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	waitSMSAdmissionSignal(t, entered)
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_resend_deferred", Status: jobs.StatusRunning, Data: map[string]any{"phoneRetryRequired": true}})
	jm.Emit(jobs.JobEvent{JobID: j.ID, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	unblock()
	waitConsoleState(t, jm, j.ID, func(j *jobs.Job) bool { return j.PhoneRetryRequired && j.SMSStatus == "error" })
	time.Sleep(20 * time.Millisecond)
	if _, exists := jm.ConsumeInput(j.ID, "phone_otp", "sms_otp", "code", "value"); exists {
		t.Fatal("an in-flight provider response submitted its code after deferral")
	}
	srv.smsMu.Lock()
	lease := srv.smsLeases[j.ID]
	safe := lease != nil && lease.retryPaused && lease.pendingCode == "" && !lease.submitted
	srv.smsMu.Unlock()
	if !safe || counts.polls.Load() != 1 || counts.numbers.Load() != 1 || counts.released.Load() != 0 || counts.completed.Load() != 0 {
		t.Fatal("in-flight response altered the paused lease or started more polling")
	}
}
