package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func activationEvidenceFixture(source string) map[string]any {
	out := map[string]any{
		"operation_id": "synthetic-operation", "account_id": "synthetic-account", "email": "owner@example.test",
		"old_totp_factor_id": "old-factor", "new_totp_factor_id": "new-factor",
		"secret": resetFixtureSecret, "session_id": "synthetic-enrollment",
		"enroll_attempted": true, "enrollment_validated": true,
		"enrollment_factor":    map[string]any{"id": "new-factor", "factor_type": "totp", "is_recovery": false},
		"activation_attempted": true, "activation_attempt_id": "synthetic-activation-attempt",
		"activation_succeeded": true, "activation_verified": true,
		"activation_evidence_schema": 2, "activation_confirmation_source": source,
		"activation_acknowledged": source == "business_receipt", "activation_confirmed_by_read": source == "state_read",
	}
	out["activation_receipt"] = map[string]any{
		"version": 1, "step": "activation", "operation_id": out["operation_id"],
		"attempt_id": out["activation_attempt_id"], "http_status": 200, "body_complete": true,
		"termination": "complete", "body_base64": base64.StdEncoding.EncodeToString([]byte(`{"success":true}`)),
		"received_at": "2026-11-07T00:00:00Z",
	}
	out["activation_evidence"] = resetActivationEvidence(out)
	return out
}

func TestActivationEvidenceAcceptsActualConfirmationSources(t *testing.T) {
	for _, source := range []string{"business_receipt", "state_read"} {
		t.Run(source, func(t *testing.T) {
			out := activationEvidenceFixture(source)
			if source == "state_read" {
				// A response-lost activation is established by the matching live read.
				delete(out, "activation_receipt")
			}
			if !verifiedResetActivation(out) {
				t.Fatal("valid source-aware completion evidence rejected")
			}
			if (out["activation_confirmed_by_read"] == true) != (source == "state_read") {
				t.Fatal("business receipt was misrepresented as a status read")
			}
			data, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var stored map[string]any
			if json.Unmarshal(data, &stored) != nil || !verifiedResetActivation(stored) {
				t.Fatal("persisted evidence lost its confirmation source")
			}
		})
	}
}

func TestAcknowledgedResetActivationRequiresCompleteBoundReceipt(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any, map[string]any)
	}{
		{"missing receipt", func(o, _ map[string]any) { delete(o, "activation_receipt") }},
		{"wrong version", func(_, r map[string]any) { r["version"] = 2 }},
		{"fractional version", func(_, r map[string]any) { r["version"] = 1.5 }},
		{"wrong step", func(_, r map[string]any) { r["step"] = "enroll" }},
		{"wrong operation", func(_, r map[string]any) { r["operation_id"] = "another-operation" }},
		{"wrong attempt", func(_, r map[string]any) { r["attempt_id"] = "another-attempt" }},
		{"missing attempt", func(o, r map[string]any) { delete(o, "activation_attempt_id"); delete(r, "attempt_id") }},
		{"blank attempt", func(o, r map[string]any) { o["activation_attempt_id"], r["attempt_id"] = " ", " " }},
		{"blank operation", func(o, r map[string]any) { o["operation_id"], r["operation_id"] = " ", " " }},
		{"missing account", func(o, _ map[string]any) { delete(o, "account_id") }},
		{"missing email", func(o, _ map[string]any) { delete(o, "email") }},
		{"missing session", func(o, _ map[string]any) { delete(o, "session_id") }},
		{"missing factor", func(o, _ map[string]any) { delete(o, "new_totp_factor_id") }},
		{"unvalidated enrollment", func(o, _ map[string]any) { o["enrollment_validated"] = false }},
		{"not attempted", func(o, _ map[string]any) { o["activation_attempted"] = false }},
		{"proven unsent", func(o, _ map[string]any) { o["mutation_schema"] = 1; o["activation_dispatch"] = "proven_unsent" }},
		{"incomplete body", func(_, r map[string]any) { r["body_complete"] = false }},
		{"interrupted response", func(_, r map[string]any) { r["termination"] = "interrupted" }},
		{"missing termination", func(_, r map[string]any) { delete(r, "termination") }},
		{"http failure", func(_, r map[string]any) { r["http_status"] = 500 }},
		{"http redirect", func(_, r map[string]any) { r["http_status"] = 302 }},
		{"fractional http status", func(_, r map[string]any) { r["http_status"] = 200.5 }},
		{"invalid base64", func(_, r map[string]any) { r["body_base64"] = "invalid!" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := activationEvidenceFixture("business_receipt")
			tc.edit(out, payload(out["activation_receipt"]))
			if acknowledgedResetActivation(out) || verifiedResetActivation(out) {
				t.Fatal("invalid receipt authorized completion")
			}
		})
	}
}

func TestAcknowledgedResetActivationRejectsUnknownAndContradictoryBodies(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"success":false}`, `{"success":"true"}`,
		`{"success":true`, `{"success":false,"success":true}`,
		`{"success":true,"error":{"code":"invalid_code"}}`,
		`{"success":true,"factor_id":"another-factor"}`,
		`{"success":true,"session_id":"another-session"}`,
		`{"message":"success"}`, `{} {"success":true}`,
		strings.Repeat(" ", maxResetReceiptBody) + `{"success":true}`,
	} {
		out := activationEvidenceFixture("business_receipt")
		payload(out["activation_receipt"])["body_base64"] = base64.StdEncoding.EncodeToString([]byte(body))
		if acknowledgedResetActivation(out) || verifiedResetActivation(out) {
			t.Fatal("unknown or contradictory activation body authorized completion")
		}
	}
}

func TestActivationEvidenceRejectsChangedBindingOrFabricatedSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"account", func(o map[string]any) { o["account_id"] = "other-account" }},
		{"email", func(o map[string]any) { o["email"] = "other@example.test" }},
		{"seed", func(o map[string]any) { o["secret"] = "JBSWY3DPEHPK3PXA" }},
		{"session", func(o map[string]any) { o["session_id"] = "other-session" }},
		{"source", func(o map[string]any) { o["activation_confirmation_source"] = "guessed" }},
		{"proof source", func(o map[string]any) { payload(o["activation_evidence"])["confirmation_source"] = "state_read" }},
		{"proof attempt", func(o map[string]any) { payload(o["activation_evidence"])["attempt_id"] = "other-attempt" }},
		{"proof missing", func(o map[string]any) { delete(o, "activation_evidence") }},
		{"false acknowledgement", func(o map[string]any) { o["activation_acknowledged"] = false }},
		{"invented query", func(o map[string]any) { o["activation_confirmed_by_read"] = true }},
		{"unsupported schema", func(o map[string]any) { o["activation_evidence_schema"] = 4 }},
		{"unverified", func(o map[string]any) { o["activation_verified"] = false }},
		{"not succeeded", func(o map[string]any) { o["activation_succeeded"] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := activationEvidenceFixture("business_receipt")
			tc.edit(out)
			if verifiedResetActivation(out) {
				t.Fatal("changed evidence authorized completion")
			}
		})
	}
}

func TestActivationEvidencePreservesReadOnlyLegacyCompatibility(t *testing.T) {
	for _, schema := range []int{0, 1} {
		out := activationEvidenceFixture("state_read")
		delete(out, "activation_confirmation_source")
		delete(out, "activation_attempt_id")
		delete(out, "activation_receipt")
		if schema == 0 {
			delete(out, "activation_evidence_schema")
			delete(out, "activation_evidence")
		} else {
			out["activation_evidence_schema"] = schema
			out["activation_evidence"] = resetActivationEvidence(out)
		}
		if !verifiedResetActivation(out) {
			t.Fatal("legacy matching live-read evidence rejected")
		}
		out["activation_confirmed_by_read"] = false
		if verifiedResetActivation(out) {
			t.Fatal("legacy receipt was promoted without a recorded matching read")
		}
	}
	// A newly reconciled legacy operation has no invented activation attempt ID.
	out := activationEvidenceFixture("state_read")
	delete(out, "activation_attempt_id")
	delete(out, "activation_receipt")
	out["activation_evidence"] = resetActivationEvidence(out)
	if !verifiedResetActivation(out) {
		t.Fatal("reconciled legacy operation required an invented attempt ID")
	}
}
