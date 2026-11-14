package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/store"
)

func writeSubscriptionGrant(t *testing.T, st *store.Store, job *Job, accountID, plan string) string {
	t.Helper()
	private, err := store.New(filepath.Join(st.Root, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	creds := map[string]any{"email": job.Email, "account_id": accountID, "access_token": "synthetic-plan-access-secret", "expires_at": int64(2000000000)}
	if plan != "" {
		creds["plan_type"], creds["subscription_expires_at"] = plan, "2030-01-01T00:00:00Z"
	}
	if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": creds}}}); err != nil {
		t.Fatal(err)
	}
	digest, _, err := subscriptionAuthorizationBinding(st.Root, job.ID, job.Email)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func subscriptionManagerFixture(t *testing.T) (*Manager, *store.Store, *Job, string) {
	t.Helper()
	m, st := accountQueryManager(t)
	job := accountQueryJob(t, m, "subscription", "subscription@example.test")
	digest := writeSubscriptionGrant(t, st, job, "workspace-one", "plus")
	m.UpdateResult(job.ID, true, "synthetic existing authorization")
	date := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := m.SetManualExpiryContext(t.Context(), job.ID, &date); err != nil {
		t.Fatal(err)
	}
	job, _ = m.Get(job.ID)
	return m, st, job, digest
}

func subscriptionObservationFixture() SubscriptionObservation {
	return SubscriptionObservation{PlanType: "pro", Source: "chatgpt_accounts", ObservedAt: time.Now().UTC(), AccountID: "workspace-one"}
}

func TestSubscriptionCommitPreservesAuthorizationAndExecutionHistory(t *testing.T) {
	m, st, job, digest := subscriptionManagerFixture(t)
	vault := &expiryForbiddenVault{}
	m.SetCredentialStore(vault)
	path := filepath.Join(st.Root, job.ID, "sub2api-import-oauth.json")
	authorization, _ := os.ReadFile(path)
	before, _ := m.Get(job.ID)
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	observation := subscriptionObservationFixture()
	got, err := lease.CommitSubscription(t.Context(), observation, digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanType != "pro" || got.SubscriptionExpiresAt != "" || got.ManualExpiresAt != before.ManualExpiresAt || got.SubscriptionPlanSource != observation.Source || got.SubscriptionPlanStale || got.SubscriptionPlanCheckedAt == nil || !got.SubscriptionPlanCheckedAt.Equal(observation.ObservedAt) {
		t.Fatal("live plan or independent manual expiry was not committed")
	}
	after, _ := m.Get(job.ID)
	if after.Attempts != before.Attempts || after.Status != before.Status || after.Operation != before.Operation || after.OperationStatus != before.OperationStatus || !after.LastOperationAt.Equal(before.LastOperationAt) || after.Prompt != before.Prompt || len(m.pending) != 0 || vault.calls != 0 {
		t.Fatal("subscription commit changed execution or touched protected credentials")
	}
	authorizationAfter, _ := os.ReadFile(path)
	if !bytes.Equal(authorization, authorizationAfter) {
		t.Fatal("query rewrote the authorization file")
	}
	public, _ := json.Marshal(got)
	metadata, _ := os.ReadFile(st.Path("job-meta.json"))
	if strings.Contains(string(public), digest) || strings.Contains(string(public), "workspace-one") || strings.Contains(string(public), "synthetic-plan-access-secret") || strings.Contains(string(metadata), "synthetic-plan-access-secret") || !strings.Contains(string(metadata), digest) {
		t.Fatal("private binding or credentials escaped their intended boundary")
	}
	if got.subscriptionPlanDigest != "" || got.subscriptionPlanAccountID != "" || after.subscriptionPlanDigest != digest {
		t.Fatal("sanitization lost private metadata or exposed it publicly")
	}
}

func TestSubscriptionLiveObservationSurvivesOldClaimsRestartAndDiscovery(t *testing.T) {
	m, st, job, digest := subscriptionManagerFixture(t)
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	if _, err := lease.CommitSubscription(t.Context(), subscriptionObservationFixture(), digest); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	m.UpdateResult(job.ID, true, "same saved authorization")
	if err := m.SyncOutputs(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	current, _ := m.Get(job.ID)
	if current.PlanType != "pro" || current.SubscriptionExpiresAt != "" || current.SubscriptionPlanStale {
		t.Fatal("same authorization claims overwrote the live observation")
	}
	restored := NewManager(t.Context(), 1, st, nil)
	defer restored.cancel()
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ = restored.Get(job.ID)
	if current.PlanType != "pro" || current.SubscriptionExpiresAt != "" || current.SubscriptionPlanStale || current.SubscriptionPlanCheckedAt == nil || current.subscriptionPlanDigest != digest {
		t.Fatal("restart reintroduced stale plus claims or expiry")
	}
}

func TestSubscriptionAuthorizationReplacementInvalidatesLiveObservation(t *testing.T) {
	for _, scenario := range []string{"same-account", "different-workspace"} {
		for _, restart := range []bool{false, true} {
			t.Run(scenario+map[bool]string{false: "/update", true: "/restart"}[restart], func(t *testing.T) {
				m, st, job, digest := subscriptionManagerFixture(t)
				lease := accountQueryLease(t, m, t.Context(), job.ID)
				if _, err := lease.CommitSubscription(t.Context(), subscriptionObservationFixture(), digest); err != nil {
					t.Fatal(err)
				}
				lease.Release()
				account, plan, want := "workspace-one", "free", "free"
				if scenario == "different-workspace" {
					account, plan, want = "workspace-two", "", "unknown"
				}
				writeSubscriptionGrant(t, st, job, account, plan)
				reader := m
				if restart {
					reader = NewManager(t.Context(), 1, st, nil)
					defer reader.cancel()
					if err := reader.Restore(t.Context()); err != nil {
						t.Fatal(err)
					}
				} else {
					m.UpdateResult(job.ID, true, "new authorization snapshot")
				}
				current, _ := reader.Get(job.ID)
				if !current.SubscriptionPlanStale || current.PlanType != want || current.SubscriptionPlanCheckedAt != nil || current.SubscriptionPlanSource != "" {
					t.Fatal("new authorization inherited an authoritative live observation")
				}
				if scenario == "different-workspace" && current.SubscriptionExpiresAt != "" {
					t.Fatal("previous workspace expiry survived replacement")
				}
			})
		}
	}
}

func TestSubscriptionCommitRejectsSupersededAndQuarantinedQueries(t *testing.T) {
	for _, scenario := range []string{"relogin-queued", "proxy-change", "grant-replaced", "wrong-account", "not-saved", "totp-quarantine", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			m, st, job, digest := subscriptionManagerFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			lease := accountQueryLease(t, m, ctx, job.ID)
			observation := subscriptionObservationFixture()
			want := ErrAccountQuerySuperseded
			switch scenario {
			case "relogin-queued":
				if _, err := m.QueueOperation(job.ID, "relogin", nil); err != nil {
					t.Fatal(err)
				}
			case "proxy-change":
				if _, err := m.Update(job.ID, map[string]any{"proxyUrl": "http://proxy.example.test:8080"}); err != nil {
					t.Fatal(err)
				}
			case "grant-replaced":
				writeSubscriptionGrant(t, st, job, "workspace-two", "team")
			case "wrong-account":
				observation.AccountID = "workspace-two"
			case "not-saved":
				m.mu.Lock()
				m.jobs[job.ID].ResultSaved = false
				m.mu.Unlock()
			case "totp-quarantine":
				m.mu.Lock()
				quarantineTOTP(m.jobs[job.ID])
				m.mu.Unlock()
				want = ErrTOTPRecoveryPending
			case "cancelled":
				cancel()
				want = context.Canceled
			}
			before, _ := m.Get(job.ID)
			if _, err := lease.CommitSubscription(ctx, observation, digest); !errors.Is(err, want) {
				t.Fatalf("unsafe observation accepted: %v", err)
			}
			after, _ := m.Get(job.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected query changed saved history")
			}
		})
	}
}

func TestSubscriptionUnknownAndInvalidResultsPreserveKnownPlan(t *testing.T) {
	for _, scenario := range []string{"unknown-plan", "empty-plan", "invalid-source", "missing-time", "invalid-expiry"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, job, digest := subscriptionManagerFixture(t)
			lease := accountQueryLease(t, m, t.Context(), job.ID)
			observation := subscriptionObservationFixture()
			switch scenario {
			case "unknown-plan":
				observation.PlanType = "unknown-tier"
			case "empty-plan":
				observation.PlanType = ""
			case "invalid-source":
				observation.Source = "raw-token-response"
			case "missing-time":
				observation.ObservedAt = time.Time{}
			case "invalid-expiry":
				value := "not-a-date"
				observation.ExpiresAt = &value
			}
			before, _ := m.Get(job.ID)
			if _, err := lease.CommitSubscription(t.Context(), observation, digest); !errors.Is(err, ErrSubscriptionObservationInvalid) {
				t.Fatal("invalid observation accepted", err)
			}
			after, _ := m.Get(job.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("invalid query replaced a known plan or expiry")
			}
		})
	}
}

func TestSubscriptionMetadataPersistenceDistinguishesRejectionAndPublishedUncertainty(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "write-rejected", true: "published-but-durability-unknown"}[uncertain], func(t *testing.T) {
			m, st, job, digest := subscriptionManagerFixture(t)
			lease := accountQueryLease(t, m, t.Context(), job.ID)
			before, _ := m.Get(job.ID)
			beforeDisk, _ := os.ReadFile(st.Path("job-meta.json"))
			m.metadataWriter = func(ctx context.Context, name string, value any) error {
				if uncertain {
					if err := st.WriteJSON(ctx, name, value); err != nil {
						return err
					}
					return errors.Join(store.ErrDurabilityUncertain, os.ErrPermission)
				}
				return os.ErrPermission
			}
			_, err := lease.CommitSubscription(t.Context(), subscriptionObservationFixture(), digest)
			m.metadataWriter = nil
			after, _ := m.Get(job.ID)
			afterDisk, _ := os.ReadFile(st.Path("job-meta.json"))
			if !uncertain {
				if !errors.Is(err, ErrSubscriptionPersistence) || !reflect.DeepEqual(before, after) || !bytes.Equal(beforeDisk, afterDisk) || !lease.Valid() {
					t.Fatal("rejected save did not restore previous state", err)
				}
				if _, err := lease.CommitSubscription(t.Context(), subscriptionObservationFixture(), digest); err != nil {
					t.Fatal("same lease could not retry safe local save", err)
				}
			} else {
				if !errors.Is(err, ErrSubscriptionPersistenceUncertain) || after.PlanType != "pro" || bytes.Equal(beforeDisk, afterDisk) {
					t.Fatal("visible replacement was treated as an unapplied write", err)
				}
				restored := NewManager(t.Context(), 1, st, nil)
				defer restored.cancel()
				if err := restored.Restore(t.Context()); err != nil {
					t.Fatal(err)
				}
				current, _ := restored.Get(job.ID)
				if current.PlanType != "pro" || current.SubscriptionPlanStale || current.SubscriptionPlanCheckedAt == nil {
					t.Fatal("restart lost the published live observation")
				}
			}
		})
	}
}

func TestSubscriptionMissingAuthorizationKeepsOnlyStaleHistory(t *testing.T) {
	m, st, job, digest := subscriptionManagerFixture(t)
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	if _, err := lease.CommitSubscription(t.Context(), subscriptionObservationFixture(), digest); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if err := os.Remove(filepath.Join(st.Root, job.ID, "sub2api-import-oauth.json")); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(t.Context(), 1, st, nil)
	defer restored.cancel()
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := restored.Get(job.ID)
	if current.ResultSaved || !current.SubscriptionPlanStale || current.PlanType != "pro" || current.SubscriptionPlanCheckedAt == nil || current.SubscriptionPlanSource != "chatgpt_accounts" {
		t.Fatal("missing authorization was presented as a fresh live observation")
	}
}

func TestSubscriptionVerifiedExpiryAndCloningPreserveIndependentValues(t *testing.T) {
	m, _, job, digest := subscriptionManagerFixture(t)
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	observation := subscriptionObservationFixture()
	expiry := "2032-02-03T08:00:00+08:00"
	observation.ExpiresAt = &expiry
	got, err := lease.CommitSubscription(t.Context(), observation, digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubscriptionExpiresAt != "2032-02-03T00:00:00Z" || got.ManualExpiresAt != job.ManualExpiresAt {
		t.Fatal("provider expiry replaced manual expiry or retained timezone ambiguity")
	}
	*got.SubscriptionPlanCheckedAt = time.Time{}
	current, _ := m.Get(job.ID)
	if current.SubscriptionPlanCheckedAt.IsZero() {
		t.Fatal("public observation pointer changed live manager state")
	}
}

func testSubscriptionUnknownReplacementExpiry(t *testing.T, accountID string) {
	t.Helper()
	m, st, job, digest := subscriptionManagerFixture(t)
	lease := accountQueryLease(t, m, t.Context(), job.ID)
	observation := subscriptionObservationFixture()
	oldExpiry := "2035-01-01T00:00:00Z"
	observation.ExpiresAt = &oldExpiry
	if _, err := lease.CommitSubscription(t.Context(), observation, digest); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	writeSubscriptionGrant(t, st, job, accountID, "")
	path := filepath.Join(st.Root, job.ID, "sub2api-import-oauth.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	account := document["accounts"].([]any)[0].(map[string]any)
	account["credentials"].(map[string]any)["subscription_expires_at"] = "2044-01-01T00:00:00Z"
	private, err := store.New(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := private.WriteJSON(t.Context(), filepath.Base(path), document); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, true, "replacement with unknown plan")
	current, _ := m.Get(job.ID)
	if !current.SubscriptionPlanStale || current.ManualExpiresAt != job.ManualExpiresAt {
		t.Fatal("replacement lost staleness or manual expiry")
	}
	if accountID == observation.AccountID {
		if current.PlanType != observation.PlanType || current.SubscriptionExpiresAt != oldExpiry || current.SubscriptionPlanCheckedAt == nil || !current.SubscriptionPlanCheckedAt.Equal(observation.ObservedAt) || current.SubscriptionPlanSource != observation.Source {
			t.Fatal("unknown plan date was attached to the previous live observation")
		}
	} else if current.PlanType != "unknown" || current.SubscriptionExpiresAt != "" || current.SubscriptionPlanCheckedAt != nil || current.SubscriptionPlanSource != "" {
		t.Fatal("unknown workspace plan adopted an unconfirmed date or old provenance")
	}
	restored := NewManager(t.Context(), 1, st, nil)
	defer restored.cancel()
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	afterRestart, _ := restored.Get(job.ID)
	if afterRestart.PlanType != current.PlanType || afterRestart.SubscriptionExpiresAt != current.SubscriptionExpiresAt || afterRestart.SubscriptionPlanSource != current.SubscriptionPlanSource || !reflect.DeepEqual(afterRestart.SubscriptionPlanCheckedAt, current.SubscriptionPlanCheckedAt) || !afterRestart.SubscriptionPlanStale {
		t.Fatal("restart mixed unknown replacement expiry into the retained observation")
	}
}

func TestSubscriptionSameAccountUnknownPlanPreservesEntireHistoricalObservation(t *testing.T) {
	testSubscriptionUnknownReplacementExpiry(t, "workspace-one")
}

func TestSubscriptionDifferentWorkspaceUnknownPlanDiscardsUnconfirmedExpiry(t *testing.T) {
	testSubscriptionUnknownReplacementExpiry(t, "workspace-two")
}
