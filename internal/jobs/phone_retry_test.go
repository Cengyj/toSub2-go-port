package jobs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestPhoneRetryPauseAtomicallyDiscardsAutomaticInputs(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), 1, st, nil)
	j, err := m.Submit(Spec{ID: "phone-retry", Email: "owner@example.test", Input: map[string]any{
		"phone": "+12025550123", "phone_otp": "654321", "sms_otp": "654321", "code": "654321", "value": "654321", "password": "private-password-canary",
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Intentionally leave the subscriber full and unread. Canonical job state
	// must reject automatic work even when the SMS subscriber misses the event.
	events, unsubscribe := m.Subscribe(1)
	defer unsubscribe()
	m.Emit(JobEvent{JobID: j.ID, Stage: "phone_sent", Status: StatusRunning})
	m.Emit(JobEvent{JobID: j.ID, Stage: "phone_send_deferred", Status: StatusRunning, Data: map[string]any{
		"phoneRetryRequired": true, "currentPhone": "+12025550123", "phoneError": "temporary service error", "apiKey": "private-api-canary", "code": "654321",
	}})
	m.Emit(JobEvent{JobID: j.ID, Stage: "sms_otp", Status: StatusWaitingInput})
	current, _ := m.Get(j.ID)
	if !current.PhoneRetryRequired || current.CurrentPhone != "+12025550123" || len(events) != 1 {
		t.Fatal("retry pause was not applied before subscriber delivery")
	}
	if m.InputAutomaticSMS(j.ID, "112233") {
		t.Fatal("automatic SMS was accepted while protocol requires manual recovery")
	}
	if _, exists := m.ConsumeInput(j.ID, "phone", "phone_otp", "sms_otp", "code", "value"); exists {
		t.Fatal("deferred request retained an old phone or OTP input candidate")
	}
	if password, exists := m.ConsumeInput(j.ID, "password"); !exists || password != "private-password-canary" {
		t.Fatal("retry pause removed an unrelated login credential")
	}
	if !m.InputForStage(j.ID, "phone_otp", map[string]any{"phone_otp": "112233"}) {
		t.Fatal("explicit manual recovery input was rejected")
	}
	if code, exists := m.ConsumeInput(j.ID, "phone_otp"); !exists || code != "112233" {
		t.Fatal("explicit manual recovery code was not delivered")
	}
	current, _ = m.Get(j.ID)
	if !current.PhoneRetryRequired {
		t.Fatal("submitting a manual code resumed automatic SMS before protocol acceptance")
	}
	var metadata any
	if err := st.ReadJSON(context.Background(), "job-meta.json", &metadata); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(metadata)
	if !strings.Contains(string(encoded), "\"phone_retry_required\":true") || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "654321") || strings.Contains(string(encoded), "112233") {
		t.Fatal("metadata lost the safe retry flag or persisted private input")
	}
	m.Emit(JobEvent{JobID: j.ID, Stage: "sms_resent", Status: StatusRunning, Data: map[string]any{"phoneRetryRequired": false}})
	m.Emit(JobEvent{JobID: j.ID, Stage: "sms_otp", Status: StatusWaitingInput})
	if !m.InputAutomaticSMS(j.ID, "445566") {
		t.Fatal("successful explicit resend did not allow fresh automatic SMS")
	}
}

func TestPhoneRetryPauseRestoresFromMetadataAndCheckpoint(t *testing.T) {
	for _, source := range []string{"snake_metadata", "camel_metadata", "checkpoint"} {
		t.Run(source, func(t *testing.T) {
			const id, email, phone = "phone-restart", "owner@example.test", "+12025550123"
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			record := map[string]any{"id": id, "email": email, "status": "phone_otp", "current_phone": phone}
			switch source {
			case "snake_metadata":
				record["phone_retry_required"] = true
			case "camel_metadata":
				record["phoneRetryRequired"] = true
			}
			if err := st.WriteJSON(context.Background(), "job-meta.json", map[string]any{"jobs": []any{record}}); err != nil {
				t.Fatal(err)
			}
			jobStore, err := store.New(filepath.Join(st.Root, id))
			if err != nil {
				t.Fatal(err)
			}
			if err := jobStore.WriteJSON(context.Background(), "login-checkpoint.json", map[string]any{
				"version": 1, "email": email, "stage": "phone_required", "oauth": map[string]any{"phone": phone, "phoneRetryRequired": source == "checkpoint"},
			}); err != nil {
				t.Fatal(err)
			}
			m := NewManager(context.Background(), 1, st, nil)
			if err := m.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			j, exists := m.Get(id)
			if !exists || !j.PhoneRetryRequired || j.CurrentPhone != phone || j.Status != "resume_available" {
				t.Fatal("restart lost the original phone or manual retry requirement")
			}
			m.Emit(JobEvent{JobID: id, Stage: "sms_otp", Status: StatusWaitingInput})
			if m.InputAutomaticSMS(id, "654321") {
				t.Fatal("restored retry pause accepted automatic SMS")
			}
			// prepareRestoredJob also handles existing in-memory private input
			// during imports. Old configured phone/code aliases must not replay.
			j.Input = map[string]any{"phone": phone, "phone_otp": "654321", "sms_otp": "654321", "code": "654321", "value": "654321", "password": "keep-private-password"}
			if err := m.prepareRestoredJob(context.Background(), id, j, nil); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"phone", "phone_otp", "sms_otp", "code", "value"} {
				if _, exists := j.Input[key]; exists {
					t.Fatalf("restored retry pause retained automatic input %s", key)
				}
			}
			if j.Input["password"] != "keep-private-password" {
				t.Fatal("restore discarded unrelated private login input")
			}
		})
	}
}

func TestPhoneRetryEventOnlyAcceptsSafeBoolean(t *testing.T) {
	for _, value := range []any{true, false, "true", "private-canary", 1, map[string]any{"secret": "private-canary"}} {
		event := SanitizeJobEvent(JobEvent{Data: map[string]any{"phoneRetryRequired": value, "code": "654321", "apiKey": "private-canary"}})
		_, want := value.(bool)
		if got, exists := event.Data["phoneRetryRequired"]; exists != want || (exists && got != value) || len(event.Data) != map[bool]int{true: 1, false: 0}[want] {
			t.Fatal("retry metadata did not enforce its public boolean-only contract")
		}
	}
}

func TestPhoneRejectionDiscardsConfiguredPhoneCandidates(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, err := m.Submit(Spec{ID: "phone-rejected", Email: "owner@example.test", Input: map[string]any{"phone": "+12025550123", "code": "654321", "value": "+12025550123", "password": "preserve-login-password"}})
	if err != nil {
		t.Fatal(err)
	}
	m.Emit(JobEvent{JobID: j.ID, Stage: "phone_rejected", Status: StatusRunning, Data: map[string]any{"phoneRetryRequired": false}})
	m.Emit(JobEvent{JobID: j.ID, Stage: "phone", Status: StatusWaitingInput})
	if _, exists := m.ConsumeInput(j.ID, "phone", "code", "value"); exists {
		t.Fatal("rejected configured phone was automatically resubmitted")
	}
	if password, exists := m.ConsumeInput(j.ID, "password"); !exists || password != "preserve-login-password" {
		t.Fatal("phone rejection discarded unrelated login password")
	}
	if !m.InputForStage(j.ID, "phone", map[string]any{"phone": "+12025550999"}) {
		t.Fatal("phone rejection prevented explicit replacement input")
	}
}
