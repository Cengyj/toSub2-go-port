package protocol

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestResetLostActivationReceiptRequiresMatchingCurrentFactor(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		reads      int
	}{
		{"another-session-current", resetInfo("another-session-factor"), 200, 1},
		{"status-unavailable", `{"error":"temporary"}`, 503, totpStatusChecks},
		{"session-revoked", `{"detail":{"code":"token_revoked"}}`, 401, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newResetFixture(t, true)
				reads := 0
				x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
					if step == "activate" {
						return response(200, `{}`), nil, true
					}
					if strings.HasPrefix(step, "info") && step != "info1" {
						reads++
						return response(tc.status, tc.body), nil, true
					}
					return nil, nil, false
				}
				out, err := x.flow.ResetTOTP(t.Context(), x.path)
				if err == nil || reads != tc.reads || !x.pending || x.persisted || out["activation_acknowledged"] == true || acknowledgedResetActivation(out) {
					t.Fatalf("uncertain activation bypassed current-factor confirmation: reads=%d err=%v", reads, err)
				}
				if out["reset_totp_status"] != "unknown" || out["reset_totp_phase"] != "manual_review_required" || out["activation_verified"] != false || out["secret"] != resetFixtureSecret {
					t.Fatal("uncertain current factor was committed or lost its protected material")
				}
				for _, step := range []string{"disable", "enroll", "activate"} {
					if countResetSteps(x.calls, step) != 1 {
						t.Fatalf("%s repeated after activation", step)
					}
				}
				if countResetSteps(x.calls, "logout") != 0 {
					t.Fatal("unconfirmed current factor caused logout")
				}
				if _, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email); err == nil {
					t.Fatal("unknown activation authorized local completion")
				}
				before := len(x.calls)
				if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
					t.Fatal("incomplete read check authorized restarting security mutations")
				}
			})
		})
	}
}

// Both server-side removals acknowledge the same F0, matching the live sample.
// Both original executions continue. Neither receipt describes the final list.
func TestResetConcurrentReceiptsCommitOnlyOwnEnrollment(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	allRemoved, aActivated, allActivated := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	removed := 0
	current := "old-factor"
	fixtures := []*resetFixture{newResetFixture(t, false), newResetFixture(t, false)}
	fixtures[1].seed = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for i, x := range fixtures {
		x.override = func(step string, req transport.Request) (*transport.Response, error, bool) {
			wait := func(ch <-chan struct{}) error {
				select {
				case <-ch:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			switch {
			case strings.HasPrefix(step, "info"):
				mu.Lock()
				id := current
				mu.Unlock()
				return response(200, resetInfo(id)), nil, true
			case step == "disable":
				assertResetBody(t, req, map[string]any{"factor_id": "old-factor"})
				mu.Lock()
				removed++
				if removed == 2 {
					current = ""
					close(allRemoved)
				}
				mu.Unlock()
				if err := wait(allRemoved); err != nil {
					return nil, err, true
				}
				return response(200, `{"message":"Factor disabled","passkey_credential_manager_sync":null}`), nil, true
			case step == "enroll":
				return response(200, fmt.Sprintf(`{"secret":%q,"session_id":"session-%d","factor":{"id":"new-factor-%d","factor_type":"totp","is_recovery":false}}`, x.seed, i, i)), nil, true
			case step == "activate":
				if !x.pending {
					t.Error("activation preceded durable pending material")
				}
				if i == 1 {
					if err := wait(aActivated); err != nil {
						return nil, err, true
					}
				}
				mu.Lock()
				current = fmt.Sprintf("new-factor-%d", i)
				mu.Unlock()
				if i == 0 {
					close(aActivated)
				} else {
					close(allActivated)
				}
				if err := wait(allActivated); err != nil {
					return nil, err, true
				}
				return response(200, `{"success":true}`), nil, true
			}
			return nil, nil, false
		}
	}
	type result struct {
		out map[string]any
		err error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, x := range fixtures {
		wg.Go(func() { results[i].out, results[i].err = x.flow.ResetTOTP(ctx, x.path) })
	}
	wg.Wait()
	for i, x := range fixtures {
		if readResetResult(t, x.path)["secret"] != x.seed || results[i].out["operation_id"] == results[1-i].out["operation_id"] {
			t.Fatal("mixed operation or seed")
		}
		if results[i].out["old_factor_removal_acknowledged"] != true || results[i].out["activation_acknowledged"] != true || !x.pending {
			t.Fatalf("execution %d did not continue after acknowledged shared removal: %v", i, results[i].err)
		}
		if !reflect.DeepEqual(x.calls, []string{"page", "session", "info1", "disable", "enroll", "activate"}) {
			t.Fatalf("execution %d replayed a mutation or queried after its receipt: %v", i, x.calls)
		}
		if results[i].err != nil || !x.persisted || results[i].out["activation_confirmation_source"] != "business_receipt" || results[i].out["activation_confirmed_by_read"] == true || results[i].out["new_totp_factor_id"] != fmt.Sprintf("new-factor-%d", i) || results[i].out["session_id"] != fmt.Sprintf("session-%d", i) {
			t.Fatalf("execution %d lost its own receipt or claimed a list match", i)
		}
	}
}

func TestResetStaleFactorGeneric400StopsWithoutReplacingTarget(t *testing.T) {
	x := newResetFixture(t, false)
	x.override = func(step string, req transport.Request) (*transport.Response, error, bool) {
		if step == "disable" {
			assertResetBody(t, req, map[string]any{"factor_id": "old-factor"})
			x.remoteFactor = "another-session-factor"
			return response(400, `{"detail":"Invalid request"}`), nil, true
		}
		return nil, nil, false
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err == nil || x.pending || x.persisted || out["reset_totp_status"] != "failed" {
		t.Fatal("generic stale-ID rejection was promoted to success or unknown")
	}
	if !reflect.DeepEqual(x.calls, []string{"page", "session", "info1", "disable"}) || x.remoteFactor != "another-session-factor" {
		t.Fatal("stale-ID failure reselected or modified another session's factor")
	}
}

func TestSchemaThreeAlwaysRequiresDatedReadAndKeepsLegacyCompletion(t *testing.T) {
	legacy := activationEvidenceFixture("business_receipt")
	if !verifiedResetActivation(legacy) {
		t.Fatal("historical receipt evidence can no longer complete local storage")
	}
	legacy["activation_evidence_schema"] = 3
	legacy["activation_evidence"] = resetActivationEvidence(legacy)
	if verifiedResetActivation(legacy) {
		t.Fatal("new operation accepted receipt-only completion")
	}
	out := activationEvidenceFixture("state_read")
	out["activation_evidence_schema"] = 3
	out["activation_state_checked_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	out["activation_evidence"] = resetActivationEvidence(out)
	if !verifiedResetActivation(out) {
		t.Fatal("new dated current-factor evidence rejected")
	}
	out["activation_state_checked_at"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if verifiedResetActivation(out) {
		t.Fatal("read time was not bound to activation evidence")
	}
}
