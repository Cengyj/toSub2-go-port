package sub2api

import (
	"errors"
	"strings"
	"time"
	_ "time/tzdata" // Keep timezone validation available in standalone Go binaries.
)

const DefaultOpenAIRequestTimezone = "America/Los_Angeles"

var ErrInvalidOpenAIRequestTimezone = errors.New("Sub2 请求时区无效，请填写有效的 IANA 时区，例如 America/Los_Angeles")

// NormalizeOpenAIRequestTimezone supplies the default and rejects local-machine
// aliases and paths, so the configured timezone behaves the same on every host.
func NormalizeOpenAIRequestTimezone(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return DefaultOpenAIRequestTimezone, nil
	}
	if name == "Local" || len(name) > 200 || strings.HasPrefix(name, "/") {
		return "", ErrInvalidOpenAIRequestTimezone
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", ErrInvalidOpenAIRequestTimezone
		}
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/_-+", r) {
			continue
		}
		return "", ErrInvalidOpenAIRequestTimezone
	}
	if _, err := time.LoadLocation(name); err != nil {
		return "", ErrInvalidOpenAIRequestTimezone
	}
	return name, nil
}
