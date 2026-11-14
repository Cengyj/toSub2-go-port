package protocol

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

// These fixtures deliberately keep authorization out of the page. The live
// session response owns both the selected identity and its current web token.
func webAuthorizationSessionBody(token string) string {
	return `{"user":{"email":"enroll@example.com"},"account":{"id":"current-account"},"accessToken":"` + token + `"}`
}

func TestTOTPWebAuthorizationUsesLiveSessionToken(t *testing.T) {
	for _, tc := range []struct{ name, page string }{
		{"html-without-token", `<html><body>Application shell</body></html>`},
		{"stale-html-token", `{"accessToken":"stale-page-token-must-never-be-used"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newResetFixture(t, false)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				switch step {
				case "page":
					return response(http.StatusOK, tc.page), nil, true
				case "session":
					return response(http.StatusOK, webAuthorizationSessionBody(resetFixtureToken)), nil, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || out["credential_persisted"] != true || out["activation_verified"] != true {
				t.Fatal("live session token did not complete the selected reset", err)
			}
			if !reflect.DeepEqual(x.calls[:2], []string{"page", "session"}) {
				t.Fatal("authorization did not retain the read-only page/session order", x.calls)
			}
			for _, step := range []string{"disable", "enroll", "activate"} {
				if countResetSteps(x.calls, step) != 1 {
					t.Fatal("successful authorization replayed a mutation", x.calls)
				}
			}
		})
	}
}

func TestTOTPWebAuthorizationMissingTokenNeverStartsMutation(t *testing.T) {
	x := newResetFixture(t, false)
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		if step == "page" {
			return response(http.StatusOK, `<html><body>Application shell</body></html>`), nil, true
		}
		return nil, nil, false
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err == nil || !strings.HasPrefix(err.Error(), "TOTP_RESET_NOT_STARTED:") {
		t.Fatal("missing web authorization did not remain a not-started reset", err)
	}
	if !reflect.DeepEqual(x.calls, []string{"page", "session"}) {
		t.Fatal("missing page token prevented live session resolution or reached a mutation", x.calls)
	}
	if x.intent || x.pending || x.persisted || out["old_factor_invalidated"] != false || out["reset_totp_phase"] != "initial" || out["disable_dispatch"] != "not_sent" {
		t.Fatal("missing authorization changed the credential or factor boundary")
	}
	if ValidateTOTPResetStart(x.path, x.flow.Options.Email) != nil {
		t.Fatal("not-started read failure blocked a future explicit attempt")
	}
}

func TestTOTPWebAuthorizationIdentityFailureCannotUseHTMLFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":"session_expired"}`},
		{"malformed-json", http.StatusOK, `<html>Session unavailable</html>`},
		{"missing-email", http.StatusOK, `{"account":{"id":"current-account"},"accessToken":"` + resetFixtureToken + `"}`},
		{"different-email", http.StatusOK, `{"user":{"email":"other@example.com"},"account":{"id":"current-account"},"accessToken":"` + resetFixtureToken + `"}`},
		{"missing-account", http.StatusOK, `{"user":{"email":"enroll@example.com"},"accessToken":"` + resetFixtureToken + `"}`},
		{"inconsistent-accounts", http.StatusOK, `{"user":{"email":"enroll@example.com"},"account":{"id":"current-account","account_id":"different-account"},"accessToken":"` + resetFixtureToken + `"}`},
		{"duplicate-email", http.StatusOK, `{"user":{"email":"other@example.com","email":"enroll@example.com"},"account":{"id":"current-account"},"accessToken":"` + resetFixtureToken + `"}`},
		{"duplicate-account", http.StatusOK, `{"user":{"email":"enroll@example.com"},"account":{"id":"different-account","id":"current-account"},"accessToken":"` + resetFixtureToken + `"}`},
		{"duplicate-user", http.StatusOK, `{"user":{"email":"other@example.com"},"user":{"email":"enroll@example.com"},"account":{"id":"current-account"},"accessToken":"` + resetFixtureToken + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newResetFixture(t, false)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == "session" {
					return response(tc.status, tc.body), nil, true
				}
				return nil, nil, false
			}
			_, _, err := x.flow.currentTOTPAuthorization(t.Context(), resetFixtureToken)
			if err == nil {
				t.Fatal("rejected live identity did not stop before factor mutation", err)
			}
			if !reflect.DeepEqual(x.calls, []string{"session"}) || x.intent || x.pending || x.persisted {
				t.Fatal("page fallback bypassed live identity validation", x.calls)
			}
			if strings.Contains(err.Error(), resetFixtureToken) {
				t.Fatal("authorization failure disclosed a token")
			}
		})
	}
}

func TestTOTPWebAuthorizationInitialLoginStillRequiresLiveIdentity(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing-email", `{"account":{"id":"current-account"},"accessToken":"` + resetFixtureToken + `"}`},
		{"different-email", `{"user":{"email":"other@example.com"},"account":{"id":"current-account"},"accessToken":"` + resetFixtureToken + `"}`},
		{"missing-account", `{"user":{"email":"enroll@example.com"},"accessToken":"` + resetFixtureToken + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFirstResetLoginFixture(t, true)
			baseline := f.reset.override
			postLoginReads := 0
			f.reset.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
				if step == "page" {
					return response(http.StatusOK, `{"accessToken":"`+resetFixtureToken+`"}`), nil, true
				}
				if step == "session" && f.authenticated {
					postLoginReads++
					return response(http.StatusOK, tc.body), nil, true
				}
				return baseline(step, r)
			}
			out, err := f.reset.flow.ResetTOTP(t.Context(), f.reset.path)
			if err == nil || !strings.HasPrefix(err.Error(), "TOTP_RESET_NOT_STARTED:") || f.login.loginStarts != 1 || f.anonymousReads != 0 || postLoginReads == 0 {
				t.Fatal("initial 401 did not authenticate once and stop on an unconfirmed live identity", err)
			}
			if f.reset.intent || f.reset.pending || f.reset.persisted || f.reset.info != 0 || out["old_factor_invalidated"] != false || out["reset_totp_phase"] != "initial" || out["disable_dispatch"] != "not_sent" {
				t.Fatal("login or page token bypassed the live identity boundary")
			}
			assertPreparedNoMutations(t, f.reset.calls)
			if strings.Contains(err.Error(), resetFixtureToken) {
				t.Fatal("identity failure disclosed a token")
			}
		})
	}
}
