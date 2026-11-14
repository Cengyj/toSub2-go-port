package main

import (
	"context"
	"errors"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/mail"
)

func TestOperationInputConsumesStageAliasesOnce(t *testing.T) {
	for _, tc := range []struct{ stage, alias, value, want string }{
		{"password", "password", " \tSynthetic Password9! ", " \tSynthetic Password9! "},
		{"email_otp", "emailOtp", " 123456 ", "123456"},
		{"totp", "mfa_otp", " 234567 ", "234567"},
		{"sms_otp", "phone_otp", " 345678 ", "345678"},
		{"totp_setup", "totp_setup_otp", " 456789 ", "456789"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			cfg := sessionAuditConfig(t.TempDir(), "")
			manager, _, _ := sessionAuditManager(t, cfg, credentials.NewMemoryStore())
			job, err := manager.Submit(jobs.Spec{ID: "input", Email: "input@example.test", Input: map[string]any{tc.alias: tc.value}})
			if err != nil {
				t.Fatal(err)
			}
			var events []jobs.JobEvent
			var password string
			input := newOperationInput(cfg, manager, job.ID, func(event jobs.JobEvent) { events = append(events, event) }, "", mail.RequestConfig{}, func(value string) { password = value })
			value, err := input.read(t.Context(), tc.stage, "synthetic prompt")
			if err != nil || value != tc.want {
				t.Fatalf("stage input changed: %q, %v", value, err)
			}
			if _, exists := manager.ConsumeInput(job.ID, tc.alias); exists {
				t.Fatal("accepted code remained available for replay")
			}
			if len(events) != 1 || events[0].Status != jobs.StatusWaitingInput || events[0].Stage != tc.stage || events[0].Prompt != "synthetic prompt" {
				t.Fatal("manual input emitted incorrect or duplicate waiting events")
			}
			if tc.stage == "password" && password != tc.want || tc.stage != "password" && password != "" {
				t.Fatal("password update crossed input stages")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			value, err = input.read(ctx, tc.stage, "retry prompt")
			if !errors.Is(err, context.Canceled) || value != "" || len(events) != 2 {
				t.Fatal("canceled repeat input reused a value or emitted extra events", err)
			}
		})
	}
}
