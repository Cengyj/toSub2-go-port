package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestSetupManualActivationRetainsUnverifiedSeedAcrossRestart(t *testing.T) {
	x := newSetupOutcomeFixture(t)
	prior := x.flow.Client.Transport
	activations := 0
	x.flow.Client.Transport = doerFunc(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
		if strings.HasSuffix(request.URL, "/mfa/enroll") {
			record, err := ReadTOTPResetJournal(x.path)
			if err != nil || record["enrollment_dispatch"] != "may_have_been_sent" || record["activation_dispatch"] != "not_sent" || record["activation_succeeded"] != false {
				t.Fatal("enrollment was dispatched before intent durability and readback", err)
			}
		}
		if strings.HasSuffix(request.URL, "/activate_enrollment") {
			activations++
			record, err := ReadTOTPResetJournal(x.path)
			if err != nil || record["activation_dispatch"] != "may_have_been_sent" || record["seed_verified"] != false {
				t.Fatal("activation was dispatched without its own safe intent", err)
			}
			if activations == 1 {
				x.activation = response(400, `{"error":{"code":"invalid_code"}}`)
			} else {
				x.activation = response(200, `{"success":true}`)
				if record["activation_mode"] != "manual" {
					t.Fatal("manual code was labeled automatic before submission")
				}
			}
		}
		return prior.Do(ctx, request)
	})
	x.flow.Options.Input = func(context.Context, string, string) (string, error) { return "123456", nil }
	out, err := x.flow.SetupTOTP(t.Context(), x.path)
	if err != nil || out["activation_succeeded"] != true || SetupTOTPSeedVerified(out) || out["activation_mode"] != "manual" || activations != 2 {
		t.Fatalf("manual activation lost independent outcome: activations=%d err=%v", activations, err)
	}
	before, err := os.ReadFile(x.path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if json.Unmarshal(before, &stored) != nil || SetupTOTPSeedVerified(stored) || stored["secret"] != resetFixtureSecret || stored["session_id"] != "setup-session" {
		t.Fatal("manual activation evidence lost the pending material")
	}
	requests := len(x.calls)
	// A fresh Flow cannot restore the old login or overwrite this operation.
	fresh := NewFlow(doNotUseClient(), x.flow.Options)
	if _, err := fresh.SetupTOTP(t.Context(), x.path); err == nil || len(x.calls) != requests {
		t.Fatal("restart replayed the enrollment or activation")
	}
	after, err := os.ReadFile(x.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("restart changed the private recovery evidence")
	}
}

func TestSetupSeedProofRejectsUnknownModesAndSchemas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value map[string]any
		want  bool
	}{
		{"current-automatic", map[string]any{"activation_succeeded": true, "setup_evidence_schema": 1, "activation_mode": "automatic", "seed_verified": true}, true},
		{"legacy-automatic", map[string]any{"activation_succeeded": true, "activation_mode": "automatic"}, true},
		{"legacy-no-mode", map[string]any{"activation_succeeded": true}, true},
		{"manual", map[string]any{"activation_succeeded": true, "activation_mode": "manual", "seed_verified": true}, false},
		{"explicit-false", map[string]any{"activation_succeeded": true, "seed_verified": false}, false},
		{"missing-proof", map[string]any{"activation_succeeded": true, "setup_evidence_schema": 1, "activation_mode": "automatic"}, false},
		{"future-schema", map[string]any{"activation_succeeded": true, "setup_evidence_schema": 2, "activation_mode": "automatic", "seed_verified": true}, false},
		{"not-activated", map[string]any{"activation_succeeded": false, "activation_mode": "automatic", "seed_verified": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SetupTOTPSeedVerified(tc.value); got != tc.want {
				t.Fatalf("verified=%v want=%v", got, tc.want)
			}
		})
	}
}
