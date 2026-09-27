package credentials

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Older Keychain versions stored JSON directly and could truncate at 128 bytes.
// Recover only completely decoded top-level strings before an unexpected EOF.
// Envelopes, malformed JSON and partially decoded values are never recoverable.
func recoverTruncatedLegacyCredentials(raw string) (Credentials, bool) {
	var out Credentials
	decoder := json.NewDecoder(strings.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return Credentials{}, false
	}
	recovered := func(err error) (Credentials, bool) {
		if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && (out.Password != "" || out.TOTPSecret != "" || out.ProxyURL != "") {
			return out, true
		}
		return Credentials{}, false
	}
	seen := map[string]bool{}
	for {
		token, err := decoder.Token()
		if err != nil {
			return recovered(err)
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return Credentials{}, false
		}
		seen[key] = true
		switch key {
		case "password", "totpSecret", "proxyUrl":
			var value string
			if err := decoder.Decode(&value); err != nil {
				return recovered(err)
			}
			switch key {
			case "password":
				out.Password = value
			case "totpSecret":
				out.TOTPSecret = value
			case "proxyUrl":
				out.ProxyURL = value
			}
		case "version":
			var version int
			if err := decoder.Decode(&version); err != nil {
				return recovered(err)
			}
			if version != 1 && version != 2 {
				return Credentials{}, false
			}
		default:
			// In particular, do not reinterpret iv/tag/ciphertext envelopes.
			return Credentials{}, false
		}
	}
}
