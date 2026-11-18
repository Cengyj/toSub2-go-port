package protocol

import (
	"strings"
	"testing"
)

func TestReceiptRetainsRevocationCodeWithoutEchoedCredentials(t *testing.T) {
	body := []byte(`{"error":{"code":"token_revoked","message":"private-token-canary","token":"private-token-canary"}}`)
	projected := totpReceiptBody(body)
	if !strings.Contains(string(projected), "token_revoked") || strings.Contains(string(projected), "private-token-canary") {
		t.Fatal("revocation reason lost or credential echo retained")
	}
	out, valid := decodeTOTPResponse(projected)
	if !valid || !totpResponseRejected(out) || totpEnrollmentAlreadyExists(out) || totpInvalidCodeResponse(out) {
		t.Fatal("revocation incorrectly authorizes retry or conflict fallback")
	}
}
