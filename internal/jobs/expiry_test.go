package jobs

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/store"
)

func expiryJSONField(t *testing.T, job *Job, key string) string {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	value, _ := fields[key].(string)
	return value
}

func writeExpiryFixture(t *testing.T, root, id, email, expiry string) {
	t.Helper()
	private, err := store.New(filepath.Join(root, id))
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{"email": email, "expires_at": int64(2000000000), "access_token": "synthetic-access-token"}
	if expiry != "" {
		fields["subscription_expires_at"] = expiry
	}
	if err := private.WriteJSON(t.Context(), "sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": fields}}}); err != nil {
		t.Fatal(err)
	}
}

func TestManualExpiryRoundTripsAndSurvivesAuthorizationOperations(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	defer m.cancel()
	job, err := m.Submit(Spec{ID: "expiry-fixture", Email: "expiry@example.test", ImportOnly: true, Input: map[string]any{"manualExpiresAt": "2030-01-02T16:30:00+08:00"}})
	if err != nil {
		t.Fatal(err)
	}
	const manual = "2030-01-02T08:30:00Z"
	if expiryJSONField(t, job, "manualExpiresAt") != manual {
		t.Fatal("manual expiry was not saved as a UTC account field")
	}
	writeExpiryFixture(t, st.Root, job.ID, job.Email, "2031-02-03T08:00:00+08:00")
	m.UpdateResult(job.ID, true, "synthetic authorization saved")
	updated, _ := m.Get(job.ID)
	if expiryJSONField(t, updated, "manualExpiresAt") != manual || expiryJSONField(t, updated, "subscriptionExpiresAt") != "2031-02-03T00:00:00Z" {
		t.Fatal("authorization metadata did not retain independent manual and provider expiry")
	}
	if _, err := m.QueueOperation(job.ID, "relogin", map[string]any{"authorizationMethod": AuthorizationMethodBPS}); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(job.ID, false, "synthetic login failure")
	updated, _ = m.Get(job.ID)
	if expiryJSONField(t, updated, "manualExpiresAt") != manual || expiryJSONField(t, updated, "subscriptionExpiresAt") != "2031-02-03T00:00:00Z" {
		t.Fatal("failed login erased recorded expiry")
	}
	restored := NewManager(t.Context(), 1, st, nil)
	defer restored.cancel()
	if err := restored.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	updated, _ = restored.Get(job.ID)
	if expiryJSONField(t, updated, "manualExpiresAt") != manual || expiryJSONField(t, updated, "subscriptionExpiresAt") != "2031-02-03T00:00:00Z" {
		t.Fatal("restart lost account expiry metadata")
	}
}

func TestSubscriptionExpiryDoesNotUseAccessTokenExpiry(t *testing.T) {
	for _, test := range []struct{ name, reported, want string }{
		{"provider-subscription", "2031-01-01T00:00:00+08:00", "2030-12-31T16:00:00Z"},
		{"token-expiry-only", "", ""},
		{"invalid-provider-value", "synthetic-private-value", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			writeExpiryFixture(t, st.Root, "saved", "expiry@example.test", test.reported)
			if err := m.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			job, ok := m.Get("saved")
			if !ok || expiryJSONField(t, job, "subscriptionExpiresAt") != test.want {
				t.Fatal("subscription expiry was lost or inferred from a different expiry kind")
			}
		})
	}
}

func TestImportedManualExpiryUpdatesWithoutQueuingLogin(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	defer m.cancel()
	spec := Spec{ID: "imported-expiry", Email: "imported-expiry@example.test", ImportOnly: true, Input: map[string]any{"manualExpiresAt": "2030-01-01T00:00:00Z"}}
	job, created, err := m.SubmitUniqueEmail(spec)
	if err != nil || !created {
		t.Fatal("cannot import fixture", err)
	}
	spec.Input = map[string]any{"manualExpiresAt": "2031-02-03T04:05:00Z"}
	updated, created, err := m.SubmitUniqueEmail(spec)
	if err != nil || created || updated.ID != job.ID || expiryJSONField(t, updated, "manualExpiresAt") != "2031-02-03T04:05:00Z" {
		t.Fatal("duplicate import did not update the manual expiry", err)
	}
	spec.Input = map[string]any{"proxyUrl": ""}
	updated, _, err = m.SubmitUniqueEmail(spec)
	if err != nil || expiryJSONField(t, updated, "manualExpiresAt") != "2031-02-03T04:05:00Z" || len(m.pending) != 0 {
		t.Fatal("omitted expiry erased the stored value or import queued login")
	}
}

func TestManualExpiryRejectsUnnormalizedInputBeforeSubmission(t *testing.T) {
	for _, value := range []any{"16.30", "2030-01-01", "synthetic-private-value", nil, 2000000000} {
		m := NewManager(t.Context(), 1, nil, nil)
		defer m.cancel()
		if job, err := m.Submit(Spec{Email: "invalid-expiry@example.test", ImportOnly: true, Input: map[string]any{"manualExpiresAt": value}}); err == nil || job != nil || len(m.List()) != 0 {
			t.Fatal("unnormalized manual expiry entered durable account metadata")
		}
	}
}
