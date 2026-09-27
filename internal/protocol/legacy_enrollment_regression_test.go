package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/transport"
)

func TestLegacyTOTPActivationPersistsPendingAndSurvivesConfirmationFailure(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed_mutation_status_read_fails", true: "interrupted_activation_retains_seed"}[interrupted], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "totp-setup-result.json")
			calls := map[string]int{}
			ids := map[string]bool{}
			var events []string
			const secret = "NB2W45DFOIZAQWER"
			f := enrollmentFlow(t, doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
				u, _ := url.Parse(r.URL)
				calls[u.Path]++
				if u.Path == "/" {
					return fixtureJSON(map[string]any{"accessToken": "fixture-access-token-long-enough-for-page"}), nil
				}
				id := r.Headers.Get("Oai-Session-Id")
				if !fixtureUUID.MatchString(id) || ids[id] {
					t.Error("MFA API requires fresh per-request UUID")
				}
				ids[id] = true
				if r.Headers.Get("Authorization") != "Bearer fixture-access-token-long-enough-for-page" || r.Headers.Get("Oai-Device-Id") != "fixture-device" || r.Headers.Get("Origin") != "https://chat.example" || r.Headers.Get("Referer") != "https://chat.example/" || r.Headers.Get("X-Openai-Target-Path") != u.Path || r.Headers.Get("X-Openai-Target-Route") != u.Path {
					t.Error("MFA headers lost session context")
				}
				switch u.Path {
				case "/backend-api/accounts/mfa_info":
					if calls[u.Path] > 1 {
						return response(503, "{}"), nil
					}
					return fixtureJSON(map[string]any{"mfa_enabled_v2": false}), nil
				case "/backend-api/accounts/mfa/enroll":
					var body map[string]any
					_ = json.Unmarshal(r.Body, &body)
					if !reflect.DeepEqual(body, map[string]any{"factor_type": "totp"}) {
						t.Error("enrollment JSON changed")
					}
					return fixtureJSON(map[string]any{"secret": secret, "session_id": "fixture-enrollment"}), nil
				case "/backend-api/accounts/mfa/user/activate_enrollment":
					b, err := os.ReadFile(path)
					var pending map[string]any
					if err != nil || json.Unmarshal(b, &pending) != nil || pending["activation_succeeded"] != false || pending["secret"] != secret {
						t.Error("pending private setup result not written before activation")
					}
					private, err := os.Open(path)
					if err != nil {
						t.Error("setup material file not private")
					} else {
						if err = privatefile.Verify(private); err != nil {
							t.Error(err)
						}
						_ = private.Close()
					}
					uri, _ := url.Parse(str(pending["otpauth_uri"]))
					q := uri.Query()
					if q.Get("secret") != secret || q.Get("issuer") != "OpenAI" || q.Get("algorithm") != "SHA1" || q.Get("digits") != "6" || q.Get("period") != "30" {
						t.Error("otpauth URI lost legacy parameters")
					}
					if interrupted {
						return nil, context.Canceled
					}
					return fixtureJSON(map[string]any{"success": true}), nil
				default:
					t.Error("unexpected MFA endpoint")
					return nil, errors.New("unexpected")
				}
			}), FlowOptions{Event: func(stage, status, prompt string) { events = append(events, stage+":"+prompt) }})
			out, err := f.SetupTOTP(context.Background(), path)
			b, readErr := os.ReadFile(path)
			var persisted map[string]any
			if readErr != nil || json.Unmarshal(b, &persisted) != nil {
				t.Fatal("private result missing")
			}
			if interrupted {
				if !errors.Is(err, context.Canceled) || persisted["activation_succeeded"] != false || calls["/backend-api/accounts/mfa_info"] != 1 {
					t.Fatal("interrupted enrollment claimed success or confirmed status")
				}
			} else {
				if err != nil || out["activation_succeeded"] != true || persisted["activation_succeeded"] != true || calls["/backend-api/accounts/mfa_info"] != 2 || !strings.Contains(strings.Join(events, " "), "totp_status_pending") {
					t.Fatalf("confirmation failure broke successful activation: %v", err)
				}
			}
			if calls["/backend-api/accounts/mfa/enroll"] != 1 || calls["/backend-api/accounts/mfa/user/activate_enrollment"] != 1 {
				t.Fatal("enrollment/activation repeated")
			}
			if strings.Contains(strings.Join(events, " "), secret) || strings.Contains(strings.Join(events, " "), "fixture-access") {
				t.Fatal("sensitive setup material leaked in events")
			}
		})
	}
}

func TestLegacyAddPasswordReauthenticationEmailOTP(t *testing.T) {
	stages := []string{"r", "000000", "123456"}
	index := 0
	calls := map[string]int{}
	proofs := 0
	requested := 0
	baselines := 0
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "password-add-result.json")
	password := "QuotePassword1'"
	f := enrollmentFlow(t, doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		u, _ := url.Parse(r.URL)
		calls[u.Path]++
		switch u.Path {
		case "/api/auth/csrf":
			return fixtureJSON(map[string]any{"csrfToken": "fixture-csrf"}), nil
		case "/api/auth/signin/openai":
			if baselines != 1 {
				t.Error("password email baseline must precede sending")
			}
			q := u.Query()
			if q.Get("connection") != "password" || q.Get("reauth") != "password" || q.Get("post_login_add_password") != "true" || q.Get("max_age") != "0" || q.Get("ext-oai-did") != "fixture-device" {
				t.Error("password enrollment must request reauthentication")
			}
			form, _ := url.ParseQuery(string(r.Body))
			if form.Get("csrfToken") != "fixture-csrf" || form.Get("json") != "true" {
				t.Error("password enrollment CSRF form changed")
			}
			return fixtureJSON(map[string]any{"url": "https://auth.example/email-verification"}), nil
		case "/email-verification", "/reset-password/new-password":
			return response(200, "fixture page"), nil
		case "/api/accounts/email-otp/resend":
			if baselines != 2 {
				t.Error("resend baseline must precede sending")
			}
			if string(r.Body) != "{}" {
				t.Error("resend must be empty JSON")
			}
			return fixtureJSON(map[string]any{}), nil
		case "/api/accounts/email-otp/validate":
			if r.Headers.Get("Openai-Sentinel-Token") != "email_otp_validate" {
				t.Error("email OTP sentinel missing")
			}
			if calls[u.Path] == 1 {
				return response(401, "{\"error\":{\"message\":\"incorrect email otp code\"}}"), nil
			}
			return fixtureJSON(map[string]any{}), nil
		case "/api/accounts/password/add":
			var body map[string]any
			_ = json.Unmarshal(r.Body, &body)
			if !reflect.DeepEqual(body, map[string]any{"password": password}) || r.Headers.Get("Openai-Sentinel-Token") != "password_reset" || r.Headers.Get("Referer") != "https://auth.example/reset-password/new-password" {
				t.Error("new password request lost body or Sentinel context")
			}
			return fixtureJSON(map[string]any{"page": map[string]any{"type": "external_url"}}), nil
		default:
			t.Error("unexpected add-password request " + u.Path)
			return nil, errors.New("unexpected")
		}
	}), FlowOptions{Input: func(_ context.Context, stage, prompt string) (string, error) {
		if stage != "email_otp" || index >= len(stages) {
			return "", errors.New("unexpected input")
		}
		v := stages[index]
		index++
		return v, nil
	}, Sentinel: func(_ context.Context, origin, device, flow string) (http.Header, error) {
		proofs++
		h := http.Header{}
		h.Set("Openai-Sentinel-Token", flow)
		return h, nil
	}, BeforeEmailOTP: func(context.Context) error { baselines++; return nil }, EmailOTPRequested: func(time.Time) { requested++ }})
	f.checkpoint.Stage = "phone_required"
	f.checkpoint.OAuth = map[string]any{"state": "stale", "codeVerifier": "stale"}
	if err := f.AddPassword(context.Background(), password, resultPath); err != nil {
		t.Fatal(err)
	}
	if index != 3 || proofs != 3 || requested != 2 || calls["/api/accounts/password/add"] != 1 || calls["/reset-password/new-password"] != 1 {
		t.Fatal("wrong password enrollment sequence")
	}
	var result map[string]any
	b, err := os.ReadFile(resultPath)
	if err != nil || json.Unmarshal(b, &result) != nil || result["password"] != password {
		t.Fatal("private password result missing")
	}
	if f.checkpoint.Stage != "email_verified" || f.checkpoint.OAuth != nil {
		t.Fatal("password enrollment left consumed OAuth state")
	}
}

func TestLegacyStrongPasswordCompatibility(t *testing.T) {
	for _, password := range []string{"QuotationMark1'", "QuotationMark1\"", "UnicodeSymbol1界", "SpaceAsSymbol1 ", "LessThanSymbol1<", strings.Repeat("a", 124) + "A1😀"} {
		if !validNewPassword(password) {
			t.Fatal("legacy valid password was rejected")
		}
	}
	for _, password := range []string{"Short1!", "NoDigitsHere!", "lowercaseonly1!", "UPPERCASEONLY1!", "NoSymbolsHere1", strings.Repeat("a", 125) + "A1😀"} {
		if validNewPassword(password) {
			t.Fatal("invalid password was accepted")
		}
	}
}

func TestSafePermanentAccountFailureClassification(t *testing.T) {
	for _, state := range []string{"deactivated", "deleted", "suspended", "disabled"} {
		for _, prefix := range []string{"account_", "user_"} {
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": prefix + state, "message": "fixture-private-token"}})
			e := newHTTPError(403, b)
			if e.Code != prefix+state || strings.Contains(e.Error(), "fixture-private-token") {
				t.Fatal("permanent alias was lost or unsafe")
			}
		}
		for _, message := range []string{"Your account has been " + state + ".", "This account is " + state + ".", "Account has been permanently " + state + "."} {
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "generic_account_error", "message": message + " fixture-private-token"}})
			e := newHTTPError(403, b)
			if e.Code != "account_"+state || strings.Contains(e.Error(), "fixture-private-token") {
				t.Fatalf("explicit permanent message not normalized: %q", e.Code)
			}
		}
	}
	for _, message := range []string{"Your account has not been disabled.", "Your account may be suspended.", "Could not delete your account.", "Unknown error fixture-private-token"} {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
		e := newHTTPError(400, b)
		if strings.HasPrefix(e.Code, "account_") || strings.Contains(e.Error(), "fixture-private-token") {
			t.Fatal("ambiguous account message permanently classified or exposed")
		}
	}
}

func TestOAuthWorkspaceCheckpointAvoidsReselectingAfterInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selections := 0
	f := NewFlow(NewClient(doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		if !strings.HasSuffix(r.URL, "/workspace/select") {
			return nil, errors.New("unexpected request")
		}
		selections++
		cancel()
		return fixtureJSON(map[string]any{"continue_url": DefaultCodexRedirectURI + "?code=fixture&state=expected"}), nil
	}), nil, ""), FlowOptions{Email: "fixture@example.test"})
	cp := Checkpoint{Version: 1, Stage: "oauth_continue", Email: f.Options.Email, Cookies: []transport.Cookie{}, OAuth: map[string]any{"state": "expected", "codeVerifier": "fixture-verifier", "current": authPayload{"oai-client-auth-session": map[string]any{"workspaces": []any{map[string]any{"id": "fixture-org", "kind": "organization"}}}}}}
	if err := f.Restore(cp); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ResumeOAuth(ctx, cp); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not honored: %v", err)
	}
	b, _ := json.Marshal(f.checkpoint)
	var restored Checkpoint
	_ = json.Unmarshal(b, &restored)
	if restored.Stage != "oauth_continue" || restored.OAuth["workspaceId"] != "fixture-org" {
		t.Fatal("selected workspace not checkpointed before continuation")
	}
	next := NewFlow(f.Client, f.Options)
	if err := next.Restore(restored); err != nil {
		t.Fatal(err)
	}
	out, err := next.ResumeOAuth(context.Background(), restored)
	if err != nil || out["workspaceId"] != "fixture-org" || selections != 1 {
		t.Fatal("resume selected workspace again or lost selection")
	}
}
