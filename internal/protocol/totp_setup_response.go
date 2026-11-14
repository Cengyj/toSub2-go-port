package protocol

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"tosub2-go-port/internal/transport"
)

// Keep the existing setup headers and session. Partial bodies may contain
// useful recovery material, but only complete responses can prove success.
func (f *Flow) totpSetupMutation(ctx context.Context, token, endpoint string, body any) (map[string]any, *transport.Response, bool, error) {
	r, requestErr := f.Client.Request(ctx, http.MethodPost, f.Options.ChatGPTBase+endpoint, RequestOptions{
		Headers: mfaHeaders(f.Options.ChatGPTBase, token, f.deviceID, endpoint), JSON: body,
		Origin: f.Options.ChatGPTBase, Referer: f.Options.ChatGPTBase + "/",
		RetryBeforeSend: true, DisableChallengeReplay: true, CaptureResponse: true, MaxResponseBytes: maxResetReceiptBody,
	})
	if r == nil {
		if requestErr == nil {
			requestErr = errors.New("TOTP_RESPONSE_MISSING: no response was received")
		}
		return nil, nil, false, requestErr
	}
	result, valid := decodeTOTPResponse(r.Body)
	if r.BodyComplete {
		requestErr = nil
	}
	if requestErr != nil {
		return result, r, r.BodyComplete, requestErr
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return result, r, true, newHTTPError(r.StatusCode, r.Body)
	}
	if !valid {
		return nil, r, true, errors.New("TOTP_RESPONSE_UNKNOWN: response JSON could not be interpreted")
	}
	return result, r, true, nil
}

// Both existing setup fixtures (secret + session_id) and reset enrollment
// fixtures (also containing factor) are recognized. An optional factor, when
// supplied, is binding evidence and may not be silently ignored when invalid.
func preserveSetupEnrollment(out, enroll map[string]any, email string) (secret, sid, factorID string, err error) {
	secret, sid = normalizeTOTPSecret(str(enroll["secret"])), str(enroll["session_id"])
	if secret != "" {
		out["secret"] = secret
	}
	if sid != "" {
		out["session_id"] = sid
	}
	uri, seedErr := BuildTOTPURI(email, secret)
	if seedErr == nil {
		out["otpauth_uri"] = uri
	}
	if seedErr != nil || strings.TrimSpace(sid) == "" {
		err = errors.New("TOTP_ENROLL_INVALID: required valid Base32 seed or enrollment session is missing; enrollment will not be repeated")
	}
	if value, present := enroll["factor"]; present {
		factor := payload(value)
		factorID = str(factor["id"])
		if factorID != "" {
			out["new_totp_factor_id"] = factorID
		}
		out["enrollment_factor"] = map[string]any{"id": factorID, "factor_type": factor["factor_type"], "is_recovery": factor["is_recovery"]}
		if strings.TrimSpace(factorID) == "" || factor["factor_type"] != "totp" || factor["is_recovery"] != false {
			err = errors.New("TOTP_ENROLL_INVALID: supplied enrollment factor is incomplete or inconsistent; no activation was sent")
		}
	}
	if !totpResponseBindingMatches(enroll, factorID, sid) {
		err = errors.New("TOTP_ENROLL_INVALID: enrollment bindings are inconsistent; no activation was sent")
	}
	return
}
