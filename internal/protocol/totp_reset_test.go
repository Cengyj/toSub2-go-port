package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

const resetFixtureSecret = "JBSWY3DPEHPK3PXP"
const resetFixtureToken = "fixture-access-token-at-least-twenty-characters"

func resetInfo(id string) string {
	if id == "" {
		return `{"mfa_enabled_v2":false,"factors":{"totp":[]}}`
	}
	return fmt.Sprintf(`{"mfa_enabled_v2":true,"factors":{"totp":[{"id":%q,"factor_type":"totp","is_recovery":false}]}}`, id)
}

type resetFixture struct {
	t                          *testing.T
	flow                       *Flow
	path                       string
	calls                      []string
	info                       int
	override                   func(string, transport.Request) (*transport.Response, error, bool)
	intent, pending, persisted bool
}

func newResetFixture(t *testing.T, logout bool) *resetFixture {
	t.Helper()
	x := &resetFixture{t: t, path: filepath.Join(t.TempDir(), "totp.json")}
	checkpoint := filepath.Join(filepath.Dir(x.path), "checkpoint.json")
	x.flow = enrollmentFlow(t, doerFunc(x.request), FlowOptions{CheckpointPath: checkpoint, LogoutAllDevicesAfterTOTP: logout})
	if err := WriteCheckpoint(checkpoint, *x.flow.checkpoint); err != nil {
		t.Fatal(err)
	}
	x.flow.Client.ConnectionRetries = 3
	x.flow.Client.Challenge = func(context.Context, string) error { t.Fatal("security request ran challenge solver"); return nil }
	x.flow.Options.TOTPResetBeforeDisable = func(context.Context) error {
		saved := readResetResult(t, x.path)
		if saved["reset_totp_phase"] != "disable_intent" || saved["old_factor_invalidated"] != true || str(saved["reset_totp_attempted_at"]) == "" {
			t.Fatal("no durable intent before credential quarantine")
		}
		x.intent = true
		return nil
	}
	x.flow.Options.TOTPEnrollment = func(_ context.Context, secret, uri string) error {
		saved := readResetResult(t, x.path)
		if secret != resetFixtureSecret || saved["secret"] != secret || saved["reset_totp_phase"] != "enrolled" || !strings.HasPrefix(uri, "otpauth://") {
			t.Fatal("pending material not durable")
		}
		x.pending = true
		return nil
	}
	x.flow.Options.TOTPResetPersist = func(_ context.Context, secret, uri string) error {
		saved := readResetResult(t, x.path)
		if saved["activation_succeeded"] != true || saved["activation_verified"] != true || saved["credential_persisted"] != false || secret != resetFixtureSecret {
			t.Fatal("persist before activation verification")
		}
		x.persisted = true
		return nil
	}
	return x
}
func (x *resetFixture) request(_ context.Context, r transport.Request) (*transport.Response, error) {
	x.t.Helper()
	step := ""
	switch {
	case strings.Contains(r.URL, "?action=enable"):
		step = "page"
	case strings.HasSuffix(r.URL, "/api/auth/session"):
		step = "session"
	case strings.HasSuffix(r.URL, "/mfa_info"):
		x.info++
		step = fmt.Sprintf("info%d", x.info)
	case strings.HasSuffix(r.URL, "/disable_in_house"):
		step = "disable"
	case strings.HasSuffix(r.URL, "/mfa/enroll"):
		step = "enroll"
	case strings.HasSuffix(r.URL, "/activate_enrollment"):
		step = "activate"
	case strings.HasSuffix(r.URL, "/logout_all"):
		step = "logout"
	default:
		x.t.Fatalf("unexpected request: %s", r.URL)
	}
	x.calls = append(x.calls, step)
	if r.Method != http.MethodGet && !r.SingleAttempt {
		x.t.Fatalf("%s is replayable", step)
	}
	if step != "page" && step != "session" {
		for key, want := range map[string]string{"Accept": "*/*", "Originator": "web", "Oai-Did": "fixture-device", "Chatgpt-Account-Id": "current-account", "Authorization": "Bearer " + resetFixtureToken, "Sec-Fetch-Mode": "cors"} {
			if got := r.Headers.Get(key); got != want {
				x.t.Fatalf("%s %s=%q want %q", step, key, got, want)
			}
		}
		for _, key := range []string{"Oai-Device-Id", "Oai-Session-Id", "X-Access-Flow-Invocation-Id", "Sec-Fetch-User", "Upgrade-Insecure-Requests"} {
			if r.Headers.Get(key) != "" {
				x.t.Fatalf("%s has unexpected %s", step, key)
			}
		}
	}
	if x.override != nil {
		if response, err, ok := x.override(step, r); ok {
			return response, err
		}
	}
	switch step {
	case "page":
		return response(200, `{"accessToken":"`+resetFixtureToken+`"}`), nil
	case "session":
		return response(200, `{"account":{"id":"current-account"},"user":{"email":"enroll@example.com"}}`), nil
	case "info1":
		return response(200, resetInfo("old-factor")), nil
	case "info2":
		return response(200, resetInfo("")), nil
	case "info3":
		saved := readResetResult(x.t, x.path)
		if saved["activation_succeeded"] != true || saved["activation_verified"] != false {
			x.t.Fatal("activation success not recorded independently")
		}
		return response(200, resetInfo("new-factor")), nil
	case "disable":
		if !x.intent {
			x.t.Fatal("disable before durable parent acknowledgement")
		}
		if _, err := os.Stat(x.flow.Options.CheckpointPath); !errors.Is(err, os.ErrNotExist) {
			x.t.Fatal("checkpoint retained before disable")
		}
		assertResetBody(x.t, r, map[string]any{"factor_id": "old-factor"})
		return response(200, `{"message":"Factor disabled"}`), nil
	case "enroll":
		assertResetBody(x.t, r, map[string]any{"factor_type": "totp", "source": "settings"})
		return response(200, `{"secret":"`+resetFixtureSecret+`","session_id":"new-session","factor":{"id":"new-factor","factor_type":"totp","is_recovery":false}}`), nil
	case "activate":
		if !x.pending {
			x.t.Fatal("activation before pending seed protection")
		}
		var body map[string]any
		if json.Unmarshal(r.Body, &body) != nil || len(body) != 4 || body["factor_type"] != "totp" || body["session_id"] != "new-session" || body["source"] != "settings" || !digits6.MatchString(str(body["code"])) {
			x.t.Fatal("activation body mismatch")
		}
		return response(200, `{"success":true}`), nil
	case "logout":
		if !x.persisted {
			x.t.Fatal("logout before secure persistence")
		}
		if len(r.Body) != 0 || r.Headers.Get("Content-Type") != "" || r.Headers.Get("Content-Length") != "" {
			x.t.Fatal("logout must leave the empty untyped body's length to the transport")
		}
		if readResetResult(x.t, x.path)["logout_all_devices_status"] != "unknown" {
			x.t.Fatal("logout lacks durable uncertain intent")
		}
		return response(204, ""), nil
	}
	x.t.Fatalf("unexpected extra step %s", step)
	return nil, nil
}
func readResetResult(t *testing.T, path string) map[string]any {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		t.Fatal("invalid journal")
	}
	return out
}
func assertResetBody(t *testing.T, r transport.Request, want map[string]any) {
	t.Helper()
	var got map[string]any
	if json.Unmarshal(r.Body, &got) != nil || !reflect.DeepEqual(got, want) || r.Method != http.MethodPost || r.Headers.Get("Origin") != "https://chat.example" {
		t.Fatal("security request shape mismatch")
	}
}
func TestResetTOTPVerifiedAndDurablyPersistedBeforeOptionalLogout(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(fmt.Sprint(logout), func(t *testing.T) {
			x := newResetFixture(t, logout)
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil {
				t.Fatal(err)
			}
			if out["activation_succeeded"] != true || out["activation_verified"] != true || out["credential_persisted"] != true || out["reset_totp_phase"] != "credential_persisted" {
				t.Fatal("incomplete reset")
			}
			want := []string{"page", "session", "info1", "disable", "info2", "enroll", "activate", "info3"}
			if logout {
				want = append(want, "logout")
				if out["logout_all_devices_status"] != "succeeded" {
					t.Fatal("logout missing")
				}
			} else if out["logout_all_devices_status"] != "not_requested" {
				t.Fatal("unsolicited logout")
			}
			if !reflect.DeepEqual(x.calls, want) {
				t.Fatalf("request sequence %v", x.calls)
			}
			before := len(x.calls)
			if _, err = x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
				t.Fatal("completed reset was replayed")
			}
		})
	}
}

func TestResetTOTPSafeReadsRecoverWithoutRepeatingMutations(t *testing.T) {
	for _, target := range []string{"session", "info1", "info2", "info3"} {
		t.Run(target, func(t *testing.T) {
			x := newResetFixture(t, true)
			x.flow.Client.RetryDelay = 0
			timeouts := 0
			x.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
				if step != target || timeouts == 2 {
					return nil, nil, false
				}
				timeouts++
				if strings.HasPrefix(target, "info") {
					x.info-- // the live state has not advanced on a failed read
				}
				return nil, io.ErrUnexpectedEOF, true
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || out["credential_persisted"] != true || out["logout_all_devices_status"] != "succeeded" || timeouts != 2 {
				t.Fatalf("network fluctuation stopped reset: timeouts=%d err=%v", timeouts, err)
			}
			for _, mutation := range []string{"disable", "enroll", "activate", "logout"} {
				count := 0
				for _, step := range x.calls {
					if step == mutation {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("%s repeated %d times", mutation, count)
				}
			}
		})
	}
}

func TestResetTOTPReadExhaustionPreservesReasonAndStopsBeforeRemoval(t *testing.T) {
	x := newResetFixture(t, false)
	x.flow.Client.RetryDelay = 0
	calls := 0
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		if step == "session" {
			calls++
			return nil, context.DeadlineExceeded, true
		}
		return nil, nil, false
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err == nil || calls != 4 || !strings.Contains(err.Error(), "3") || out["reset_totp_phase"] != "initial" || out["old_factor_invalidated"] != false || x.intent {
		t.Fatalf("read exhaustion lost retry reason or touched factor: calls=%d out=%v err=%v", calls, out, err)
	}
}
func TestResetTOTPRequiresPersistenceAndReadableUnattemptedJournalBeforeNetwork(t *testing.T) {
	for _, scenario := range []string{"missing_before", "missing_persist", "missing_path", "attempted", "logout_attempted", "corrupt", "null"} {
		t.Run(scenario, func(t *testing.T) {
			x := newResetFixture(t, false)
			switch scenario {
			case "missing_before":
				x.flow.Options.TOTPResetBeforeDisable = nil
			case "missing_persist":
				x.flow.Options.TOTPResetPersist = nil
			case "missing_path":
				x.path = ""
			case "attempted":
				_ = WritePrivateJSON(x.path, map[string]any{"reset_totp_attempted_at": "fixture"})
			case "logout_attempted":
				_ = WritePrivateJSON(x.path, map[string]any{"logout_all_devices_attempted_at": "fixture"})
			case "corrupt":
				_ = os.WriteFile(x.path, []byte("incomplete"), 0600)
			case "null":
				_ = os.WriteFile(x.path, []byte("null"), 0600)
			}
			if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != 0 {
				t.Fatal("unsafe journal or absent callbacks reached account")
			}
		})
	}
}
func TestResetTOTPStopsOnEveryUncertainSecurityTransition(t *testing.T) {
	cases := []struct {
		name, step string
		status     int
		body       string
		network    bool
		last       string
		activated  bool
	}{
		{"missing_account", "session", 200, `{}`, false, "session", false},
		{"wrong_email", "session", 200, `{"account":{"id":"current-account"},"user":{"email":"other@example.com"}}`, false, "session", false},
		{"missing_factor_list", "info1", 200, `{"mfa_enabled_v2":true}`, false, "info1", false},
		{"multiple_factors", "info1", 200, `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"a","factor_type":"totp","is_recovery":false},{"id":"b","factor_type":"totp","is_recovery":false}]}}`, false, "info1", false},
		{"recovery_factor", "info1", 200, `{"mfa_enabled_v2":true,"factors":{"totp":[{"id":"a","factor_type":"totp","is_recovery":true}]}}`, false, "info1", false},
		{"disable_403", "disable", 403, `{"error":"fixture-private-error"}`, false, "disable", false},
		{"disable_redirect", "disable", 307, `{}`, false, "disable", false},
		{"disable_503", "disable", 503, `{}`, false, "disable", false},
		{"disable_connection_lost", "disable", 0, "", true, "disable", false},
		{"disable_unconfirmed", "disable", 200, `{"message":"unknown"}`, false, "disable", false},
		{"disable_not_removed", "info2", 200, resetInfo("old-factor"), false, "info2", false},
		{"disable_missing_list", "info2", 200, `{}`, false, "info2", false},
		{"enroll_connection_lost", "enroll", 0, "", true, "enroll", false},
		{"enroll_same_id", "enroll", 200, `{"secret":"JBSWY3DPEHPK3PXP","session_id":"new-session","factor":{"id":"old-factor","factor_type":"totp","is_recovery":false}}`, false, "enroll", false},
		{"enroll_recovery", "enroll", 200, `{"secret":"JBSWY3DPEHPK3PXP","session_id":"new-session","factor":{"id":"new-factor","factor_type":"totp","is_recovery":true}}`, false, "enroll", false},
		{"enroll_bad_secret", "enroll", 200, `{"secret":"!bad!","session_id":"new-session","factor":{"id":"new-factor","factor_type":"totp","is_recovery":false}}`, false, "enroll", false},
		{"activate_rejected", "activate", 400, `{"error":"invalid_code"}`, false, "activate", false},
		{"activate_false", "activate", 200, `{"success":false}`, false, "activate", false},
		{"activate_connection_lost", "activate", 0, "", true, "activate", false},
		{"verify_connection_lost", "info3", 0, "", true, "info3", true},
		{"verify_wrong_factor", "info3", 200, resetInfo("wrong-factor"), false, "info3", true},
		{"verify_old_factor", "info3", 200, resetInfo("old-factor"), false, "info3", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newResetFixture(t, true)
			x.override = func(step string, r transport.Request) (*transport.Response, error, bool) {
				if step != tc.step {
					return nil, nil, false
				}
				if tc.network {
					return nil, errors.New("fixture-private-error"), true
				}
				r2 := response(tc.status, tc.body)
				r2.Header.Set("Location", "https://chat.example/should-not-follow")
				if tc.status == 403 {
					r2.Header.Set("Cf-Mitigated", "challenge")
				}
				return r2, nil, true
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err == nil || strings.Contains(err.Error(), "fixture-private-error") || strings.Contains(err.Error(), resetFixtureSecret) {
				t.Fatal("unsafe reset error")
			}
			if x.calls[len(x.calls)-1] != tc.last || x.persisted || out["activation_succeeded"] != tc.activated || out["activation_verified"] != false || out["logout_all_devices_status"] != "skipped" {
				t.Fatalf("wrong stop state: calls=%v activated=%v verified=%v", x.calls, out["activation_succeeded"], out["activation_verified"])
			}
			saved := readResetResult(t, x.path)
			if saved["activation_succeeded"] != tc.activated {
				t.Fatal("activation success erased")
			}
			if str(saved["reset_totp_attempted_at"]) != "" {
				before := len(x.calls)
				if _, err = x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
					t.Fatal("failed mutation replayed")
				}
				if saved["reset_totp_phase"] != "recovery_required" || saved["old_factor_invalidated"] != true {
					t.Fatal("recovery state missing")
				}
			}
		})
	}
}
func TestResetTOTPStopsWhenCredentialAcknowledgementFails(t *testing.T) {
	for _, stage := range []string{"intent", "pending", "persist"} {
		t.Run(stage, func(t *testing.T) {
			x := newResetFixture(t, true)
			fail := func(context.Context, string, string) error { return errors.New("fixture-secret-storage-error") }
			switch stage {
			case "intent":
				x.flow.Options.TOTPResetBeforeDisable = func(context.Context) error { return errors.New("failed") }
			case "pending":
				x.flow.Options.TOTPEnrollment = fail
			case "persist":
				x.flow.Options.TOTPResetPersist = fail
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err == nil || out["reset_totp_phase"] != "recovery_required" || out["credential_persisted"] != false || out["logout_all_devices_status"] != "skipped" {
				t.Fatal("failed persistence not guarded")
			}
			last := map[string]string{"intent": "info1", "pending": "enroll", "persist": "info3"}[stage]
			if x.calls[len(x.calls)-1] != last {
				t.Fatalf("mutation after failed acknowledgement: %v", x.calls)
			}
		})
	}
}
func TestResetTOTPRejectsConflictingLiveAccountAndToken(t *testing.T) {
	x := newResetFixture(t, false)
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"other-account"}}`))
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		if step == "page" {
			return response(200, `{"accessToken":"header.`+claims+`.signature"}`), nil, true
		}
		return nil, nil, false
	}
	if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || !reflect.DeepEqual(x.calls, []string{"page", "session"}) {
		t.Fatal("conflicting identity authorized removal")
	}
}
func TestResetTOTPLogoutFailureNeverRepeatsResetOrErasesNewCredential(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"bad_request", 400, `{"error":{"code":"invalid_request","message":"fixture-private-response"}}`, "failed"},
		{"rejected", 403, `{}`, "failed"}, {"redirect", 307, `{}`, "failed"}, {"server_error", 503, `{}`, "unknown"}, {"error_body", 200, `{"error":"failed"}`, "unknown"}, {"html", 200, `<html>challenge</html>`, "unknown"}, {"invalid_json", 200, `{bad`, "unknown"}, {"network", 0, "", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newResetFixture(t, true)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step != "logout" {
					return nil, nil, false
				}
				if tc.status == 0 {
					return nil, errors.New("failure"), true
				}
				return response(tc.status, tc.body), nil, true
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || out["credential_persisted"] != true || out["reset_totp_status"] != "activated" || out["logout_all_devices_status"] != tc.want {
				t.Fatal("logout failure altered reset result", err)
			}
			if tc.status == 400 && !strings.Contains(str(out["logout_all_devices_error"]), "HTTP 400；错误标识 invalid_request") {
				t.Fatal("safe provider rejection reason lost")
			}
			if strings.Contains(str(out["logout_all_devices_error"]), "fixture-private-response") {
				t.Fatal("logout leaked untrusted response text")
			}
			before := len(x.calls)
			if _, err = x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
				t.Fatal("logout failure replayed security operation")
			}
		})
	}
}

func TestResetTOTPLogoutJSONErrorTruthinessMatchesOriginal(t *testing.T) {
	for _, tc := range []struct {
		body, want string
	}{
		{`{}`, "succeeded"}, {`{"success":true}`, "succeeded"},
		{`{"error":false}`, "succeeded"}, {`{"error":""}`, "succeeded"},
		{`{"error":0}`, "succeeded"}, {`{"error":null}`, "succeeded"},
		{`{"error":true}`, "unknown"}, {`{"error":"failed"}`, "unknown"},
		{`{"error":{}}`, "unknown"}, {`{"error":[]}`, "unknown"},
		{`{"success":false,"error":false}`, "unknown"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			x := newResetFixture(t, true)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == "logout" {
					return response(200, tc.body), nil, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || out["logout_all_devices_status"] != tc.want || !x.persisted || out["credential_persisted"] != true {
				t.Fatalf("logout response incorrectly classified: status=%v err=%v", out["logout_all_devices_status"], err)
			}
		})
	}
}

func TestLogoutHTTPDiagnosticDiscardsPrivateRemoteText(t *testing.T) {
	for _, tc := range []struct {
		body, code string
	}{
		{`{"error":{"code":"invalid_request","message":"fixture-private-response"}}`, "invalid_request"},
		{`{"error":{"type":"invalid_request_error","message":"fixture-private-response"}}`, "invalid_request_error"},
		{`{"detail":"fixture-private-response","error":"duplicate_content_length"}`, "duplicate_content_length"},
		{`{"error":{"code":"session_expired","message":"fixture-private-response"}}`, "session_expired"},
		{`{"error":{"code":"fixture_private_response","message":"fixture-private-response"}}`, ""},
		{`{"detail":"fixture-private-response"}`, ""},
		{`<html>fixture-private-response invalid_request</html>`, ""},
		{strings.Repeat("fixture-private-response", 4000), ""},
	} {
		t.Run(tc.code+tc.body[:min(20, len(tc.body))], func(t *testing.T) {
			got := logoutHTTPDiagnostic(400, []byte(tc.body))
			if !strings.Contains(got, "HTTP 400") || strings.Contains(got, "fixture") || strings.Contains(got, "<html>") {
				t.Fatal("unsafe or missing HTTP diagnostic")
			}
			if strings.Contains(got, "错误标识 ") != (tc.code != "") || tc.code != "" && !strings.Contains(got, "错误标识 "+tc.code) {
				t.Fatalf("wrong safe code: %s", got)
			}
		})
	}
}

func TestResetTOTPLogoutNetworkOutcomesRemainUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		err        error
	}{
		{"timeout", "请求超时", context.DeadlineExceeded},
		{"canceled", "请求已取消", context.Canceled},
		{"network", "未获得确认响应", errors.New("fixture-private-network-error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newResetFixture(t, true)
			x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
				if step == "logout" {
					return nil, tc.err, true
				}
				return nil, nil, false
			}
			out, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err != nil || out["logout_all_devices_status"] != "unknown" || out["credential_persisted"] != true {
				t.Fatal("unknown logout outcome changed the saved factor")
			}
			message := str(out["logout_all_devices_error"])
			if !strings.Contains(message, tc.want) || strings.Contains(message, "fixture-private") {
				t.Fatalf("network outcome lost or private error exposed: %s", message)
			}
		})
	}
}

func TestValidateTOTPResetStartOnlyAllowsKnownPreMutationJournal(t *testing.T) {
	clean := func() map[string]any {
		return map[string]any{"version": 1, "email": "enroll@example.com", "reset_totp_requested": true, "reset_totp_phase": "initial", "activation_succeeded": false, "activation_verified": false, "old_factor_invalidated": false, "credential_persisted": false, "reset_totp_attempted_at": nil, "logout_all_devices_attempted_at": nil}
	}
	for _, tc := range []struct {
		name, key string
		value     any
		allowed   bool
	}{
		{"clean", "", nil, true}, {"read_failed", "reset_totp_status", "failed", true},
		{"setup_secret", "secret", resetFixtureSecret, false}, {"empty_secret_evidence", "secret", "", false},
		{"activation", "activation_succeeded", true, false}, {"verified", "activation_verified", true, false},
		{"quarantined", "old_factor_invalidated", true, false}, {"saved", "credential_persisted", true, false},
		{"attempted", "reset_totp_attempted_at", "fixture", false}, {"malformed_attempt", "reset_totp_attempted_at", true, false},
		{"logout_attempted", "logout_all_devices_attempted_at", "fixture", false}, {"enrolled", "reset_totp_phase", "enrolled", false},
		{"unknown_version", "version", 2, false}, {"wrong_action", "reset_totp_requested", false, false},
		{"old_factor_evidence", "old_totp_factor_id", "fixture", false},
		{"wrong_account", "email", "other@example.com", false},
		{"normalized_account", "email", " ENROLL@EXAMPLE.COM ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "totp.json")
			record := clean()
			if tc.key != "" {
				record[tc.key] = tc.value
			}
			if err := WritePrivateJSON(path, record); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if err := ValidateTOTPResetStart(path, "enroll@example.com"); (err == nil) != tc.allowed {
				t.Fatalf("allowed=%t err=%v", tc.allowed, err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("preflight changed recovery record")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.json")
		link := filepath.Join(dir, "link.json")
		if err := WritePrivateJSON(target, clean()); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skip("symlinks unavailable")
		}
		if ValidateTOTPResetStart(link, "enroll@example.com") == nil {
			t.Fatal("symlink journal accepted")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "totp.json")
		body, err := json.Marshal(clean())
		if err != nil {
			t.Fatal(err)
		}
		// Valid JSON with trailing whitespace isolates the size limit from
		// malformed-file rejection and must leave the original evidence intact.
		body = append(body, []byte(strings.Repeat(" ", maxTOTPJournalBytes))...)
		if err = os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if ValidateTOTPResetStart(path, "enroll@example.com") == nil {
			t.Fatal("oversized journal accepted")
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() != int64(len(body)) {
			t.Fatal("oversized journal was changed")
		}
	})
	for _, body := range []string{`{}`, `null`, `{"version":1,"secret":"fixture"}`, `[1]`, `incomplete`} {
		t.Run("unknown_"+body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "totp.json")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if ValidateTOTPResetStart(path, "enroll@example.com") == nil {
				t.Fatal("unknown journal accepted")
			}
		})
	}
}
