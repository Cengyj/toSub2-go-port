package protocol

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestReceiptProjectionRetainsOutcomeAndContradictionSemantics(t *testing.T) {
	for _, body := range []string{
		`{"message":"Factor disabled","passkey_credential_manager_sync":null}`,
		`{"message":"Factor successfully disabled"}`, `{"success":true}`, `{"activated":true}`,
		`{}`, `{"success":"true"}`, `{"success":false}`, `{"activated":null}`,
		`{"success":true,"error":{"code":"invalid_code","cookies":["cookie-canary"]}}`,
		`{"success":true,"error":{"cookies":["cookie-canary"]}}`,
		`{"success":true,"error":false}`, `{"success":true,"code":["cookie-canary"]}`,
		`{"error":{"code":"invalid_totp"}}`, `{"error":{"code":"invalid_totp"},"code":"unexpected"}`,
		`{"error":{"code":"invalid_code"},"detail":" "}`, `{"error":"invalid_code","code":" "}`,
		`{"success":true,"detail":{"error":{"cookies":"cookie-canary"}}}`,
		`{"success":true,"detail":"{\"error\":{\"code\":\"invalid_code\"}}"}`,
		`{"success":true,"detail":{"arbitrary":{"cookies":"cookie-canary"}}}`,
		`{"success":true,"detail":[{"cookies":"cookie-canary"}]}`,
		`{"success":true,"detail":"cookie-canary"}`, `{"success":true,"detail":"null"}`,
		`{"success":true,"factor_id":"other-factor"}`, `{"success":true,"session_id":false}`,
		`{"success":true,"factor":{"id":"new-factor","factor_type":"totp","is_recovery":false,"cookies":"cookie-canary"}}`,
		`{"success":true,"factor":{"id":"new-factor","is_recovery":null}}`,
	} {
		original, valid := decodeTOTPResponse([]byte(body))
		if !valid {
			t.Fatal("invalid test input")
		}
		encoded := totpReceiptBody([]byte(body))
		if strings.Contains(string(encoded), "cookie-canary") {
			t.Fatalf("auth echo persisted: %s", encoded)
		}
		stored, valid := decodeTOTPResponse(encoded)
		if !valid {
			t.Fatal("projection lost valid response shape")
		}
		for _, step := range []string{"disable", "activation"} {
			if classifyTOTPMutation(step, original, "new-factor", "new-session") != classifyTOTPMutation(step, stored, "new-factor", "new-session") {
				t.Fatalf("projection changed %s outcome for %s", step, body)
			}
		}
		if totpInvalidCodeResponse(original) != totpInvalidCodeResponse(stored) {
			t.Fatalf("projection changed code retry eligibility for %s", body)
		}
	}
	for _, body := range []string{`{"success":false,"success":true}`, `{"success":true`, `<html>cookie-canary</html>`, `{} {"success":true}`} {
		if len(totpReceiptBody([]byte(body))) != 0 {
			t.Fatal("invalid raw response retained or promoted")
		}
	}
}

func TestResetReceiptsNeverPersistAuthenticationEcho(t *testing.T) {
	x := newResetFixture(t, false)
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		if step == "disable" {
			x.remoteFactor = ""
			return response(200, `{"message":"Factor disabled","cookies":["cookie-canary"],"web":{"callbackUrl":"callback-canary","codeVerifier":"verifier-canary"}}`), nil, true
		}
		if step == "activate" {
			x.remoteFactor = "new-factor"
			return response(200, `{"success":true,"cookies":"cookie-canary","extension":{"authSessionLoggingId":"session-canary"}}`), nil, true
		}
		return nil, nil, false
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err != nil || !verifiedResetActivation(out) || !x.persisted {
		t.Fatalf("normal receipt did not complete: %v", err)
	}
	want := []string{"page", "session", "info1", "disable", "enroll", "activate"}
	if !reflect.DeepEqual(x.calls, want) {
		t.Fatalf("unexpected requests: %v", x.calls)
	}
	b, err := os.ReadFile(x.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"disable", "enroll", "activation"} {
		body, err := base64.StdEncoding.DecodeString(str(payload(saved[step+"_receipt"])["body_base64"]))
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{"cookie-canary", "callback-canary", "verifier-canary", "session-canary"} {
			if strings.Contains(string(body), canary) || strings.Contains(string(b), canary) {
				t.Fatal("authentication material reached disk")
			}
		}
	}
}
