package protocol

import (
	"context"
	"encoding/json"
	"errors"

	"net/http"

	"strings"
	"testing"
	"testing/synctest"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestResetReviewKeepsReadFailureCause(t *testing.T) {
	x := newResetFixture(t, false)
	x.flow.Client.RetryDelay = 0
	x.override = func(string, transport.Request) (*transport.Response, error, bool) { return nil, context.Canceled, true }
	_, err := x.flow.waitForTOTPResetState(t.Context(), resetFixtureToken, "current-account", totpStateDisabled, "", "old-factor")
	if !errors.Is(err, context.Canceled) || len(x.calls) != 1 || x.calls[0] != "info1" {
		t.Fatal("state reconciliation lost cancellation or continued requests", err)
	}
}

func TestResetReviewKeepsReadFailureCausePrivate(t *testing.T) {
	for _, step := range []string{"session", "status"} {
		for _, kind := range []string{"canceled", "storage", "private"} {
			t.Run(step+"/"+kind, func(t *testing.T) {
				cause := errors.New("synthetic-private-reset-diagnostic")
				if kind == "canceled" {
					cause = context.Canceled
				}
				if kind == "storage" {
					cause = ErrResetStorageBlocked
				}
				x := newResetFixture(t, false)
				x.flow.Client.RetryDelay = 0
				x.override = func(string, transport.Request) (*transport.Response, error, bool) { return nil, cause, true }
				var err error
				if step == "session" {
					_, _, err = x.flow.currentTOTPAuthorization(t.Context(), resetFixtureToken)
				} else {
					_, err = x.flow.totpResetRequest(t.Context(), resetFixtureToken, "current-account", http.MethodGet, "/backend-api/accounts/mfa_info", nil)
				}
				if !errors.Is(err, cause) {
					t.Fatal("read failure lost its underlying cause", err)
				}
				if strings.Contains(err.Error(), "synthetic-private-reset-diagnostic") {
					t.Fatal("read failure exposed private diagnostics")
				}
			})
		}
	}
}

func TestResetReviewGeneratesActivationCodeAfterDurablePreparation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newResetFixture(t, false)
		delayed, checked := false, false
		enroll := x.flow.Options.TOTPEnrollment
		x.flow.Options.TOTPEnrollment = func(ctx context.Context, secret, uri string) error {
			if err := enroll(ctx, secret, uri); err != nil {
				return err
			}
			delayed = true
			time.Sleep(65 * time.Second)
			return nil
		}
		x.override = func(step string, request transport.Request) (*transport.Response, error, bool) {
			if step == "activate" {
				var body map[string]any
				if err := json.Unmarshal(request.Body, &body); err != nil {
					t.Fatal(err)
				}
				want, err := GenerateTOTP(resetFixtureSecret, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if body["code"] != want {
					t.Fatal("activation sent a code generated before durable preparation")
				}
				checked = true
			}
			return nil, nil, false
		}
		out, err := x.flow.ResetTOTP(t.Context(), x.path)
		if err != nil || !delayed || !checked || out["credential_persisted"] != true {
			t.Fatal("delayed activation did not complete", err)
		}
		if countResetSteps(x.calls, "activate") != 1 {
			t.Fatal("activation was repeated")
		}
	})
}
