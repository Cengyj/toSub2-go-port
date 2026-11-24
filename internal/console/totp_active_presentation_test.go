package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

// The account-level export lock also covers a queued reset on another retained
// row. That is a live operation, not evidence that recovery needs human action.
func TestPublicTOTPInFlightExplainsProtectionWithoutOpeningCapabilities(t *testing.T) {
	s, m := newMonitorTestServer(t)
	owner := importTOTPActionFixture(t, m, "active-display-owner", true)
	peer, err := m.Submit(jobs.Spec{ID: "active-display-peer", Email: owner.Email, ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(owner.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	assertActive := func(view map[string]any) {
		t.Helper()
		if view["totpOperationInFlight"] != true || view["totpExportBlocked"] != true {
			t.Fatal("active account protection was not identified separately from recovery")
		}
		for _, key := range []string{"canDownload", "canSetupTotp", "canResetTotp", "canCompleteTotp", "canRetry", "canResume", "canRegenerate", "canForceRelogin", "canAddPassword", "autoRepairEligible"} {
			if view[key] != false {
				t.Fatalf("presentation opened protected capability %s", key)
			}
		}
		for _, key := range []string{"totpSetupUnavailableReason", "totpResetUnavailableReason", "totpExportUnavailableReason", "autoRepairEligibilityReason"} {
			reason, _ := view[key].(string)
			if !strings.Contains(reason, "任务尚未结束") || strings.Contains(reason, "人工") || strings.Contains(reason, "结果未确认") {
				t.Fatalf("normal in-flight protection was described as failed recovery: %s=%q", key, reason)
			}
		}
	}
	for _, id := range []string{owner.ID, peer.ID} {
		view := completionPublicJob(t, s, id)
		assertActive(view)
		if id == owner.ID {
			if view["totpActiveOperationAt"] == "" || view["totpActiveOperationAt"] != view["lastOperationAt"] {
				t.Fatal("queued owner lost its own operation generation")
			}
		} else if view["totpActiveOperationAt"] != "" {
			t.Fatal("same-email peer borrowed another row's operation generation")
		}
	}
	for _, route := range []string{"/api/jobs?pageSize=10", "/api/jobs?pageSize=10&selection=compact"} {
		response := frontendRequest(s, http.MethodGet, route, nil, "test")
		var page struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
			t.Fatal("synthetic account list unavailable")
		}
		assertActive(page.Jobs[0])
	}
	if _, err := m.QueueOperation(owner.ID, "reset-2fa", nil); err == nil {
		t.Fatal("read-only presentation allowed a second reset")
	}
	if !m.Cancel(owner.ID) {
		t.Fatal("queued reset could not be canceled")
	}
	for _, id := range []string{owner.ID, peer.ID} {
		view := completionPublicJob(t, s, id)
		if view["totpOperationInFlight"] != false || view["totpActiveOperationAt"] != "" {
			t.Fatal("canceled queued reset kept an invented active operation")
		}
	}
}

func TestPublicTOTPCompletionInFlightRetainsLocalOnlyBoundary(t *testing.T) {
	s, m, job := completionAPIFixture(t, "business_receipt")
	before := completionPublicJob(t, s, job.ID)
	if before["totpCompletionPending"] != true || before["canCompleteTotp"] != true || before["totpOperationInFlight"] != false {
		t.Fatal("stopped confirmed activation did not offer only local completion")
	}
	stale, exists := m.Get(job.ID)
	if !exists {
		t.Fatal("local completion fixture disappeared")
	}
	if _, err := m.QueueOperation(job.ID, "complete-2fa", nil); err != nil {
		t.Fatal(err)
	}
	// Account rows are captured before the live safety projection. A queue
	// change in between must not offer a second local-completion action.
	staleView := s.publicJob(stale)
	if staleView["totpOperationInFlight"] != true || staleView["canCompleteTotp"] != false {
		t.Fatal("older terminal row opened local completion while its account was already owned")
	}
	ownedAt, ok := staleView["totpActiveOperationAt"].(string)
	if !ok || ownedAt == "" || ownedAt == stale.LastOperationAt.Format(time.RFC3339Nano) {
		t.Fatal("old row borrowed its earlier generation as current ownership evidence")
	}
	view := completionPublicJob(t, s, job.ID)
	if view["totpOperationInFlight"] != true || view["totpCompletionPending"] != true || view["canCompleteTotp"] != false || view["canResetTotp"] != false || view["canDownload"] != false {
		t.Fatal("queued local completion lost evidence or opened a parallel action")
	}
	if view["totpActiveOperationAt"] != view["lastOperationAt"] || view["totpActiveOperationAt"] != ownedAt {
		t.Fatal("current completion generation did not agree across list projections")
	}
	for _, key := range []string{"totpCompletionUnavailableReason", "totpResetUnavailableReason"} {
		reason, _ := view[key].(string)
		if !strings.Contains(reason, "任务尚未结束") || strings.Contains(reason, "人工") {
			t.Fatalf("local completion described as manual recovery: %s=%q", key, reason)
		}
	}
	if !strings.Contains(view["totpCompletionHint"].(string), "不连接远端") {
		t.Fatal("local-only boundary disappeared")
	}
}

func TestPublicTOTPStaleRunningRowDoesNotBorrowPeerOwnership(t *testing.T) {
	s, m := newMonitorTestServer(t)
	owner := importTOTPActionFixture(t, m, "stale-running-owner", true)
	peer, err := m.Submit(jobs.Spec{ID: "stale-running-peer", Email: owner.Email, ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(owner.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	stale, exists := m.Get(owner.ID)
	if !exists || !m.Cancel(owner.ID) {
		t.Fatal("could not capture and cancel previous operation")
	}
	// A retained operation status cannot supply an owner. Construct the row
	// captured during execution without opening an external authentication flow.
	stale.Status, stale.OperationStatus, stale.Stage = jobs.StatusRunning, "running", "totp_reset_activate"
	if _, err := m.QueueOperation(peer.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	view := s.publicJob(stale)
	if view["totpOperationInFlight"] != true || view["totpActiveOperationAt"] != "" {
		t.Fatal("old row borrowed the peer's live operation ownership")
	}
	if view["canResetTotp"] != false || view["canCompleteTotp"] != false || view["canDownload"] != false {
		t.Fatal("stale running row opened protected capabilities")
	}
}

func TestPublicTOTPStoppedUncertaintyStillRequiresReview(t *testing.T) {
	s, _, job := recoveryAPIFixture(t)
	view := completionPublicJob(t, s, job.ID)
	if view["totpOperationInFlight"] != false || view["totpRecoveryPending"] != true || view["totpExportBlocked"] != true || view["canCompleteTotp"] != false || view["canResetTotp"] != false || view["canDownload"] != false {
		t.Fatal("stopped unconfirmed reset became active, complete or available")
	}
	if !strings.Contains(view["totpResetUnavailableReason"].(string), "需人工处理") {
		t.Fatal("actual unconfirmed recovery lost its review instruction")
	}
}
