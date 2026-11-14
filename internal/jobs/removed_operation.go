package jobs

import "strings"

// IsRemovedAuthenticationOperation recognizes retired remote recovery intents
// at every admission boundary. Ordinary fresh relogin and token reauthorization
// remain separate supported operations.
func IsRemovedAuthenticationOperation(operation string) bool {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(operation)), "_", "-") {
	case "recover-2fa", "inspect-2fa", "reauthenticate":
		return true
	default:
		return false
	}
}
