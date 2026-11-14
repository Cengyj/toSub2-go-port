package authmaterial

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var errInvalidJSON = errors.New("invalid or ambiguous JSON")
var keyNormalizer = strings.NewReplacer("_", "", "-", "", " ", "")

// ValidateUnambiguousJSON checks every object before a typed decoder can discard
// duplicate fields. It never includes remote field names or values in errors.
func ValidateUnambiguousJSON(data []byte) error { return inspectJSON(data, false) }

// Export containers intentionally preserve vendor metadata, but cannot serve as
// an alternate archive for authentication cookies or unfinished OAuth inputs.
// This rejects a contaminated export instead of deleting unknown business data.
func ContainsLoginSessionJSON(data []byte) bool {
	return inspectJSON(data, true) != nil
}

func loginSessionKey(key string) bool {
	switch strings.ToLower(keyNormalizer.Replace(key)) {
	case "cookie", "cookies", "setcookie", "cookieref", "cookiestate", "cookiesnapshot", "sessioncookies", "logincheckpoint", "loginsession", "websession", "callbackurl", "codeverifier", "codechallenge", "oauthstate", "authorizationcode", "clientauthsession", "oaiclientauthsession", "pendingoauthcode", "authsessionloggingid", "transportsession", "sessionref", "previoussessionref", "recoverycheckpoint", "preparationcheckpoint", "reauthenticationpreviouscheckpoint", "initialloginprogress":
		return true
	}
	return false
}

func inspectJSON(data []byte, rejectSessions bool) error {
	return inspectDocument(data, 0, rejectSessions)
}

func inspectDocument(data []byte, depth int, rejectSessions bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectValue(decoder, depth, rejectSessions); err != nil {
		return errInvalidJSON
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errInvalidJSON
	}
	return nil
}

func inspectValue(decoder *json.Decoder, depth int, rejectSessions bool) error {
	if depth > 64 {
		return errInvalidJSON
	}
	token, err := decoder.Token()
	if err != nil {
		return errInvalidJSON
	}
	delim, container := token.(json.Delim)
	if !container {
		// Vendor metadata sometimes wraps a JSON document in a string. A
		// complete encoded container must not become a second export channel.
		if value, ok := token.(string); ok && rejectSessions {
			encoded := []byte(strings.TrimSpace(value))
			if len(encoded) > 0 && (encoded[0] == '{' || encoded[0] == '[') && json.Valid(encoded) {
				return inspectDocument(encoded, depth+1, true)
			}
		}
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] || (rejectSessions && loginSessionKey(key)) {
				return errInvalidJSON
			}
			seen[key] = true
			if err := inspectValue(decoder, depth+1, rejectSessions); err != nil {
				return err
			}
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			return errInvalidJSON
		}
	case '[':
		for decoder.More() {
			if err := inspectValue(decoder, depth+1, rejectSessions); err != nil {
				return err
			}
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			return errInvalidJSON
		}
	default:
		return errInvalidJSON
	}
	return nil
}
