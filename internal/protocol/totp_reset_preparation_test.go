package protocol

import (
	"testing"
)

func TestResetRequiresPendingSeedPersistenceBeforeAnyMutation(t *testing.T) {
	x := newResetFixture(t, false)
	x.flow.Options.TOTPEnrollment = nil
	if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != 0 {
		t.Fatal("missing pending-seed protection allowed reset to start")
	}
}
