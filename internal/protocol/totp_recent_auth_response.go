package protocol

import (
	"bytes"
	"encoding/json"
	"io"
)

const totpRecentAuthMessage = "TOTP_RECENT_AUTH_REQUIRED: 官方要求重新验证身份后才能创建新 2FA；操作证据已保留，请人工处理"
const totpDisableRecentAuthMessage = "TOTP_RECENT_AUTH_REQUIRED: 官方拒绝了本次停用请求，要求重新验证身份；拒绝回执已保留，请人工处理"
const recentAuthPlainDetail = "User must re-authenticate to enroll/disable a factor"

func totpRecentAuthMessageForStep(step string) string {
	if step == "disable" {
		return totpDisableRecentAuthMessage
	}
	return totpRecentAuthMessage
}

// Only the two observed official refusal envelopes are proof. The plain
// detail must match exactly; arbitrary diagnostic strings are never authority.
func isRecentAuthRefusal(body []byte) bool {
	if len(body) > maxResetReceiptBody {
		return false
	}
	envelope, valid := recentAuthObject(body)
	if !valid || len(envelope) != 1 {
		return false
	}
	if raw, ok := envelope["detail"]; ok {
		var detail string
		if json.Unmarshal(raw, &detail) != nil {
			return false
		}
		if detail == recentAuthPlainDetail {
			return true
		}
		nested, valid := recentAuthObject([]byte(detail))
		if !valid || len(nested) != 1 {
			return false
		}
		envelope = nested
	}
	refusal, valid := recentAuthObject(envelope["error"])
	var code string
	return valid && json.Unmarshal(refusal["code"], &code) == nil && code == "recent_auth_required"
}

// Reject duplicate fields instead of letting the last JSON value silently
// turn a contradictory reply into authorization for another security attempt.
func recentAuthObject(body []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	object := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, exists := object[key]; exists {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		object[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	var extra any
	return object, decoder.Decode(&extra) == io.EOF
}

func receiptStatus(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case float64:
		if n == float64(int(n)) {
			return int(n)
		}
	}
	return 0
}
