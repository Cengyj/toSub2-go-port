package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func subscriptionAPIFixture(t *testing.T) (*Server, *jobs.Manager, *jobs.Job, map[string]any, string, *atomic.Int32) {
	t.Helper()
	srv, manager := newMonitorTestServer(t)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	calls := &atomic.Int32{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "synthetic upstream must not be reached", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	srv.cfg.CLI.NativeHTTP, srv.cfg.ProxyURL, srv.cfg.ProxyURLs = true, proxy.URL, nil
	job, err := manager.Submit(jobs.Spec{ID: "subscription-api", Email: "subscription@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	credential := map[string]any{"access_token": "synthetic-subscription-access", "refresh_token": "synthetic-subscription-refresh", "client_id": protocol.DefaultCodexClientID, "email": job.Email, "chatgpt_account_id": "subscription-workspace", "chatgpt_plan_type": "plus"}
	document := map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{map[string]any{"credentials": credential, "extra": map[string]any{"email": job.Email, "account_id": "subscription-workspace", "client_id": protocol.DefaultCodexClientID}}}}
	path := filepath.Join(srv.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json")
	if err := protocol.WritePrivateJSON(path, document); err != nil {
		t.Fatal(err)
	}
	manager.UpdateResult(job.ID, true, "synthetic saved grant")
	return srv, manager, job, document, path, calls
}

func TestSubscriptionRefreshAPIRejectsUnusableAuthorizationWithoutNetwork(t *testing.T) {
	for _, scenario := range []string{"expired", "missing", "invalidated", "foreign-email", "rotation-pending", "totp-pending", "busy"} {
		t.Run(scenario, func(t *testing.T) {
			srv, manager, job, document, path, calls := subscriptionAPIFixture(t)
			credential := document["accounts"].([]any)[0].(map[string]any)["credentials"].(map[string]any)
			switch scenario {
			case "expired":
				credential["expires_at"] = time.Now().Add(-time.Hour).Unix()
				if err := protocol.WritePrivateJSON(path, document); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "invalidated":
				manager.UpdateResult(job.ID, false, "synthetic invalidated grant")
			case "foreign-email":
				credential["email"] = "foreign@example.test"
				if err := protocol.WritePrivateJSON(path, document); err != nil {
					t.Fatal(err)
				}
			case "rotation-pending":
				if err := protocol.WritePrivateJSON(filepath.Join(filepath.Dir(path), ".sub2api-import-oauth.json.refresh-grant-recovery.json"), map[string]any{"pending": true}); err != nil {
					t.Fatal(err)
				}
			case "totp-pending":
				if err := manager.UpdateTOTPResetResult(job.ID, map[string]any{"reset_totp_requested": true, "old_factor_invalidated": true, "reset_totp_phase": "disable_intent"}, errors.New("synthetic unknown remote result")); err != nil {
					t.Fatal(err)
				}
			case "busy":
				if _, err := manager.QueueOperation(job.ID, "relogin", nil); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			previous, _ := manager.Get(job.ID)
			w := accountInsightAPIRequest(srv, http.MethodPost, "/api/jobs/"+job.ID+"/refresh-subscription", "{}")
			if w.Code != http.StatusConflict || calls.Load() != 0 {
				t.Fatalf("unusable authorization reached upstream: status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
			current, _ := manager.Get(job.ID)
			if current.PlanType != previous.PlanType || current.Attempts != previous.Attempts || current.Operation != previous.Operation {
				t.Fatal("refused query changed plan or operation")
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) || strings.Contains(w.Body.String(), "synthetic-subscription-") {
				t.Fatal("refused query changed or exposed authorization")
			}
		})
	}
}

func TestSubscriptionRefreshAPIRequiresExplicitEmptyObject(t *testing.T) {
	srv, _, job, _, _, calls := subscriptionAPIFixture(t)
	path := "/api/jobs/" + job.ID + "/refresh-subscription"
	for _, body := range []string{"null", "[]", "", `{"access_token":"injected"}`, `{"proxyUrl":"http://injected"}`, `{"relogin":true}`, "{} {}"} {
		if w := accountInsightAPIRequest(srv, http.MethodPost, path, body); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid action accepted: %q %d", body, w.Code)
		}
	}
	if w := accountInsightAPIRequest(srv, http.MethodGet, path, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("GET initiated subscription query")
	}
	if calls.Load() != 0 {
		t.Fatal("input validation contacted upstream")
	}
}

func TestSubscriptionRefreshPublicObservationIsCachedAndIndependentOfInsightsSwitch(t *testing.T) {
	srv, manager, job, _, path, calls := subscriptionAPIFixture(t)
	grant, err := application.LoadAccountInsightGrant(t.Context(), srv.cfg.OutputRoot, manager, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Release()
	data, _ := os.ReadFile(path)
	digest := sha256.Sum256(data)
	when := time.Now().UTC()
	_, err = grant.Lease.(*jobs.AccountQueryLease).CommitSubscription(t.Context(), jobs.SubscriptionObservation{PlanType: "pro", Source: "chatgpt_accounts", ObservedAt: when, AccountID: "subscription-workspace"}, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	grant.Release()
	w := accountInsightAPIRequest(srv, http.MethodGet, "/api/jobs/"+job.ID, "")
	var response map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal("public observation unavailable")
	}
	if response["planType"] != "pro" || response["subscriptionPlanSource"] != "chatgpt_accounts" || response["subscriptionPlanStale"] != false || response["canRefreshSubscription"] != true || response["subscriptionPlanCheckedAt"] != when.Format(time.RFC3339Nano) {
		t.Fatal("public plan provenance missing", response)
	}
	if srv.insights != nil && srv.insights.Settings().Enabled {
		t.Fatal("fixture unexpectedly enabled global insights")
	}
	if calls.Load() != 0 {
		t.Fatal("local observation read initiated upstream query")
	}
	if strings.Contains(w.Body.String(), hex.EncodeToString(digest[:])) || strings.Contains(w.Body.String(), "subscription-workspace") || strings.Contains(w.Body.String(), "synthetic-subscription-") {
		t.Fatal("private authorization binding leaked")
	}
}

func TestSubscriptionRefreshPersistenceUncertaintyIsNotReportedAsOldState(t *testing.T) {
	srv, _, job, _, _, calls := subscriptionAPIFixture(t)
	w := httptest.NewRecorder()
	srv.subscriptionRefreshError(w, job.ID, jobs.ErrSubscriptionPersistenceUncertain)
	var result map[string]any
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &result) != nil || result["outcome"] != "unknown" || result["code"] != "persistence_uncertain" || result["job"] == nil || calls.Load() != 0 {
		t.Fatal("uncertain publication presented as a definite failure", w.Body.String())
	}
}
