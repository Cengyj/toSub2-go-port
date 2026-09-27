package jobs

import (
	"strings"

	"tosub2-go-port/internal/logging"
)

// SanitizeSMSServiceLabel preserves numeric provider service identifiers, which
// may happen to contain six digits but are not one-time verification codes.
// Explicitly known credentials are still removed before the label is exposed.
func SanitizeSMSServiceLabel(label string, known ...string) string {
	for _, secret := range known {
		if secret != "" {
			label = strings.ReplaceAll(label, secret, "[REDACTED]")
		}
	}
	numeric := label != ""
	for _, r := range label {
		if r < '0' || r > '9' {
			numeric = false
			break
		}
	}
	if numeric {
		return label
	}
	return logging.RedactText(label)
}
