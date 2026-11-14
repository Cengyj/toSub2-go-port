package console

import (
	"net/http"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestTOTPCompletedIntentReplayPrecedesSingleAndBatchCapabilities(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	j := importTOTPActionFixture(t, m, "intent-api", true)
	values := map[string]any{"totpIntentId": "http-intent-123456", "resetTotp": true, "logoutAllDevicesAfterTotp": true}
	if _, err := m.QueueOperation(j.ID, "reset-2fa", values); err != nil {
		t.Fatal(err)
	}
	completeQueuedTOTPResetFixture(t, s.cfg.OutputRoot, m, vault, j.ID, completedResetSeed, "succeeded")
	m = restoreCompletedTOTPFixtureManager(t, s, vault)
	for _, route := range []string{"/api/jobs/" + j.ID + "/setup-2fa", "/api/jobs/setup-2fa-batch"} {
		body := map[string]any{"totpIntentId": "http-intent-123456", "resetTotp": true}
		if route == "/api/jobs/setup-2fa-batch" {
			body["ids"] = []string{j.ID}
		}
		response := frontendRequest(s, http.MethodPost, route, body, "test")
		got, _ := m.Get(j.ID)
		if response.Code != http.StatusOK || m.QueuePosition(j.ID) != 0 || got.TOTPResetPhase != "credential_persisted" || got.Status == jobs.StatusQueued {
			t.Fatal("late HTTP replay started another reset", response.Code, response.Body.String())
		}
	}
}
