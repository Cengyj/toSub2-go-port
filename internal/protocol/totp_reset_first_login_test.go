package protocol

import (
	"context"

	"net/http"
	"net/url"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

// An imported account has only the anonymous cookies collected by the normal
// homepage preflight. Login and every security request use local fake responses.
type firstResetLoginFixture struct {
	reset                              *resetFixture
	login                              *recoveryHarness
	authenticated                      bool
	anonymousStatus                    int
	anonymousBody                      string
	anonymousReads, authenticatedReads int
}

func newFirstResetLoginFixture(t *testing.T, completedCheckpoint bool) *firstResetLoginFixture {
	t.Helper()
	h := newRecoveryHarness(t)
	x := newResetFixture(t, false)
	x.flow.Options.Sentinel = h.flow.Options.Sentinel
	x.flow.Options.Input = h.flow.Options.Input
	x.flow.Options.Event = h.flow.Options.Event
	if !completedCheckpoint {

		x.flow.checkpoint, x.flow.web = nil, nil
		x.flow.Client.Jar = transport.NewCookieJar(transport.Cookie{Name: "oai-did", Value: "fixture-device", Domain: "chat.example", Path: "/", Secure: true, HostOnly: true})
	}
	f := &firstResetLoginFixture{reset: x, login: h, anonymousStatus: http.StatusUnauthorized, anonymousBody: `{}`}
	h.flow = x.flow
	x.flow.Client.Transport = doerFunc(h.do)
	x.flow.Client.ConnectionRetries = 0
	h.intercept = func(ctx context.Context, r transport.Request) (*transport.Response, error, bool) {
		u, err := url.Parse(r.URL)
		if err != nil {
			t.Fatal(err)
		}
		if u.Path == "/web-callback" {
			f.authenticated = true
		}
		if u.Query().Get("action") == "enable" || u.Path == "/api/auth/session" || strings.HasPrefix(u.Path, "/backend-api/accounts/") {
			if strings.HasPrefix(u.Path, "/backend-api/accounts/") && !f.authenticated {
				t.Fatal("anonymous session reached a factor request")
			}
			out, e := x.request(ctx, r)
			return out, e, true
		}
		return nil, nil, false
	}
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		switch step {
		case "page":
			return response(http.StatusOK, `<html><body>Application shell</body></html>`), nil, true
		case "session":
			if !f.authenticated {
				f.anonymousReads++
				return response(f.anonymousStatus, f.anonymousBody), nil, true
			}
			f.authenticatedReads++
			return fixtureJSON(map[string]any{
				"user":        map[string]any{"email": x.flow.Options.Email},
				"account":     map[string]any{"id": "current-account"},
				"accessToken": resetFixtureToken,
			}), nil, true
		}
		return nil, nil, false
	}
	return f
}

func TestResetTOTPFirstLoginWithOnlyAnonymousPreflightCookies(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFirstResetLoginFixture(t, false)
			f.anonymousStatus = status
			out, err := f.reset.flow.ResetTOTP(t.Context(), f.reset.path)
			if err != nil || out["activation_verified"] != true || out["credential_persisted"] != true {
				t.Fatal("imported account did not complete its first web login and reset", err)
			}
			if f.login.loginStarts != 1 || f.anonymousReads != 0 || f.authenticatedReads != 1 || f.login.oauthRequests != 0 {
				t.Fatalf("login=%d anonymous reads=%d authenticated reads=%d oauth=%d", f.login.loginStarts, f.anonymousReads, f.authenticatedReads, f.login.oauthRequests)
			}
			for _, step := range []string{"disable", "enroll", "activate"} {
				if countResetSteps(f.reset.calls, step) != 1 {
					t.Fatalf("first login repeated or skipped %s", step)
				}
			}
		})
	}
}

func TestResetTOTPFirstLoginDoesNotTreatUnconfirmedIdentityAsAnonymous(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"server-error", http.StatusInternalServerError, `{}`},
		{"not-implemented", http.StatusNotImplemented, `{}`},
		{"bad-json", http.StatusOK, `{invalid`},
		{"missing-user", http.StatusOK, `{"account":{"id":"current-account"}}`},
		{"missing-email", http.StatusOK, `{"user":{},"account":{"id":"current-account"}}`},
		{"missing-account", http.StatusOK, `{"user":{"email":"enroll@example.com"}}`},
		{"wrong-account-owner", http.StatusOK, `{"user":{"email":"other@example.test"},"account":{"id":"current-account"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFirstResetLoginFixture(t, false)
			prior := f.reset.override
			f.reset.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
				if step == "session" {
					f.authenticatedReads++
					return response(tc.status, tc.body), nil, true
				}
				return prior(step, r)
			}
			out, err := f.reset.flow.ResetTOTP(t.Context(), f.reset.path)
			if err == nil || out["reset_totp_phase"] != "initial" || f.login.loginStarts != 1 || f.authenticatedReads != 1 {
				t.Fatal("unconfirmed identity started a login or mutation", err)
			}
			assertPreparedNoMutations(t, f.reset.calls)
		})
	}
}
