package protocol

import (
	"net/url"
	"strings"
)

// An HTTP success only describes transport. These errors keep unexpected or
// contradictory business replies out of later writes and credential commits.
// They deliberately retain no provider payload, token, phone number or code.
type authenticationReceiptError struct {
	code    string
	message string
}

func (e *authenticationReceiptError) Error() string { return e.code + ": " + e.message }

func authenticationReceiptRejected(p authPayload) bool {
	if jsonTruthy(p["error"]) {
		return true
	}
	success, present := p["success"]
	return present && success != true
}

func (f *Flow) phoneSendReceipt(p authPayload) error {
	if authenticationReceiptRejected(p) {
		return &authenticationReceiptError{code: "PHONE_SEND_RESULT_REJECTED", message: "Phone request was not accepted; retry manually, change number or cancel"}
	}
	next := continuation(p)
	if next == "" && pageType(p) == "phone_otp_verification" {
		return nil
	}
	u, err := url.Parse(next)
	base, baseErr := url.Parse(f.Options.AuthBase + "/")
	if err == nil && baseErr == nil && next != "" && strings.TrimSpace(next) == next && u.User == nil && u.Fragment == "" && u.Opaque == "" {
		u = base.ResolveReference(u)
		_, queryErr := url.ParseQuery(u.RawQuery)
		if queryErr == nil && sameOrigin(u, base) && u.Path == "/phone-verification" && (pageType(p) == "" || pageType(p) == "phone_otp_verification") {
			return nil
		}
	}
	return &authenticationReceiptError{code: "PHONE_SEND_RESULT_UNKNOWN", message: "Phone response did not confirm the verification step; retry manually, change number or cancel"}
}

func (f *Flow) phoneVerificationReceipt(p authPayload, ref string) error {
	verified, hasVerified := p["verified"]
	if authenticationReceiptRejected(p) || hasVerified && verified != true {
		return &authenticationReceiptError{code: "PHONE_OTP_RESULT_REJECTED", message: "SMS verification was not accepted; enter a code manually, resend or change number"}
	}
	if needsPhone(p) || pageType(p) == "phone_otp_verification" || pathOf(continuation(p)) == "/phone-verification" {
		return &authenticationReceiptError{code: "PHONE_OTP_RESULT_UNKNOWN", message: "SMS response did not leave phone verification; enter a code manually, resend or change number"}
	}
	// The endpoint supports a direct verification receipt, a workspace list,
	// or a bound next step. No receipt may complete a paid provider order when
	// its continuation contradicts or leaves this authentication session.
	if continuation(p) != "" {
		if f.authenticationAdvanced(p, "phone_otp_verification", ref) {
			return nil
		}
	} else if verified == true || len(slice(payload(p["oai-client-auth-session"])["workspaces"])) > 0 {
		return nil
	}
	return &authenticationReceiptError{code: "PHONE_OTP_RESULT_UNKNOWN", message: "SMS response did not confirm verification; enter a code manually, resend or change number"}
}
