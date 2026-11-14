package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

type failingTrashSMSStore struct {
	credentials.Store
	err error
}

func (s failingTrashSMSStore) Delete(string) error { return s.err }

func TestTrashCleanupHTTPPreservesSafeFailureStage(t *testing.T) {
	for _, stage := range []string{"insights", "quality", "sms"} {
		t.Run(stage, func(t *testing.T) {
			s, manager, _ := trashConsole(t)
			job, err := manager.Submit(jobs.Spec{ID: "cleanup-diagnostic", Email: "cleanup@example.invalid", ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = manager.TrashAccounts(t.Context(), []string{job.ID}); err != nil {
				t.Fatal(err)
			}
			entry := manager.ListTrash()[0]
			var expected error
			switch stage {
			case "insights":
				original := s.insights
				t.Cleanup(func() { s.insights = original })
				s.insights = nil
				expected = jobs.ErrTrashInsightCleanup
			case "quality":
				original := s.quality
				t.Cleanup(func() { s.quality = original })
				s.quality = nil
				expected = jobs.ErrTrashQualityCleanup
			case "sms":
				original := s.monitorVault
				t.Cleanup(func() { s.monitorVault = original })
				s.monitorVault = failingTrashSMSStore{Store: original, err: errors.New("/private/synthetic-account token=private-synthetic-token")}
				expected = jobs.ErrTrashSMSCleanup
			}
			body, _ := json.Marshal(map[string]any{"ids": []string{entry.ID}, "confirmPermanent": true})
			response := trashRequest(s, http.MethodPost, "/api/account-trash/purge", string(body))
			var receipt struct {
				Outcome      string `json:"outcome"`
				PurgePending bool   `json:"purgePending"`
				Error        string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusConflict || receipt.Outcome != "partial" || !receipt.PurgePending || !strings.Contains(receipt.Error, expected.Error()) || !strings.Contains(receipt.Error, "不可恢复") {
				t.Fatalf("failure stage or committed purge state lost: %d %+v", response.Code, receipt)
			}
			retained, exists := manager.GetTrash(entry.ID)
			if !exists || retained.State != jobs.TrashStatePurging || retained.LastError != expected.Error() {
				t.Fatalf("cleanup failure was not retained safely: %+v", retained)
			}
			listing := trashRequest(s, http.MethodGet, "/api/account-trash", "")
			for _, output := range []string{response.Body.String(), listing.Body.String()} {
				if strings.Contains(output, "/private/") || strings.Contains(output, "private-synthetic-token") {
					t.Fatal("private cleanup cause exposed in public response")
				}
			}
		})
	}
}

func TestTrashCleanupBusySMSIsRetryableWithoutStorageWarning(t *testing.T) {
	s, manager, _ := trashConsole(t)
	job, err := manager.Submit(jobs.Spec{ID: "cleanup-busy", Email: "busy@example.invalid", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.TrashAccounts(t.Context(), []string{job.ID}); err != nil {
		t.Fatal(err)
	}
	entry := manager.ListTrash()[0]
	s.smsMu.Lock()
	s.smsAcquiring[job.ID] = true
	s.smsMu.Unlock()
	_, err = manager.PurgeTrash(t.Context(), []string{entry.ID})
	if !errors.Is(err, jobs.ErrTrashBusy) || strings.Contains(err.Error(), "存储") {
		t.Fatalf("busy SMS mislabeled as a storage failure: %v", err)
	}
	s.smsMu.Lock()
	delete(s.smsAcquiring, job.ID)
	s.smsMu.Unlock()
	if err := s.recycleBin.Sweep(t.Context(), time.Now()); err != nil || len(manager.ListTrash()) != 0 {
		t.Fatalf("already committed cleanup did not resume after SMS released: %v", err)
	}
}

func TestTrashSMSCleanupPreservesTimeoutWithoutPrivateDetails(t *testing.T) {
	s, _, _ := trashConsole(t)
	original := s.monitorVault
	t.Cleanup(func() { s.monitorVault = original })
	s.monitorVault = failingTrashSMSStore{Store: original, err: errors.Join(context.DeadlineExceeded, errors.New("/private/synthetic-vault"))}
	err := s.purgeTrashSMS(t.Context(), &jobs.TrashEntry{State: jobs.TrashStatePurging, Jobs: []*jobs.Job{{ID: "synthetic-sms"}}})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, jobs.ErrTrashSMSCleanup) || !errors.Is(err, errSMSTrashPurge) || strings.Contains(err.Error(), "/private/") {
		t.Fatalf("SMS timeout lost its category or exposed private data: %v", err)
	}
}
