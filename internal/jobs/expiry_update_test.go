package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

type expiryForbiddenVault struct{ calls int }

func (v *expiryForbiddenVault) Load(string) (credentials.Credentials, error) {
	v.calls++
	return credentials.Credentials{}, errors.New("synthetic vault unavailable")
}
func (v *expiryForbiddenVault) Save(string, credentials.Credentials) error {
	v.calls++
	return errors.New("synthetic vault unavailable")
}
func (v *expiryForbiddenVault) Delete(string) error {
	v.calls++
	return errors.New("synthetic vault unavailable")
}

func TestManualExpiryMetadataEditsDoNotTouchCredentialsOrExecution(t *testing.T) {
	for _, method := range []string{"setter", "update", "duplicate-import", "duplicate-active"} {
		t.Run(method, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			vault := &expiryForbiddenVault{}
			m.SetCredentialStore(vault)
			job := &Job{ID: "active-expiry", Email: "active-expiry@example.test", Status: StatusQueued, Operation: "relogin", AuthorizationMethod: AuthorizationMethodBPS, OperationStatus: "queued", ResultSaved: true, HasPassword: true, TOTPRecoveryPending: true, SubscriptionExpiresAt: "2030-01-01T00:00:00Z", Input: map[string]any{"password": "synthetic-password"}}
			m.jobs[job.ID] = job
			m.pending = []*Job{job}
			canceled := false
			if method == "duplicate-active" {
				job.Status, job.OperationStatus = "phone", "waiting"
				m.pending = nil
				m.active[job.ID] = func() { canceled = true }
			}
			before := cloneJob(job)
			pending := append([]*Job(nil), m.pending...)
			events, unsubscribe := m.Subscribe(2)
			defer unsubscribe()
			value := time.Date(2031, 2, 3, 4, 5, 0, 0, time.FixedZone("fixture", 8*3600))
			values := map[string]any{"manualExpiresAt": value.Format(time.RFC3339)}
			switch method {
			case "setter":
				_, err = m.SetManualExpiryContext(t.Context(), job.ID, &value)
			case "update":
				_, err = m.Update(job.ID, values)
			default:
				_, _, err = m.SubmitUniqueEmail(Spec{Email: job.Email, ImportOnly: method == "duplicate-import", Input: values})
			}
			if err != nil {
				t.Fatal(err)
			}
			after, _ := m.Get(job.ID)
			if expires, source := EffectiveExpiry(after); expires != "2031-02-02T20:05:00Z" || source != "manual" {
				t.Fatal("manual expiry precedence was not applied")
			}
			after.ManualExpiresAt, after.UpdatedAt = before.ManualExpiresAt, before.UpdatedAt
			if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(m.pending, pending) || canceled || m.restart[job.ID] || len(events) != 0 || len(m.wake) != 0 || vault.calls != 0 {
				t.Fatal("metadata edit touched credentials, eligibility, queue, or active authentication")
			}
			cleared, err := m.SetManualExpiryContext(t.Context(), job.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if expires, source := EffectiveExpiry(cleared); expires != before.SubscriptionExpiresAt || source != "subscription" || vault.calls != 0 {
				t.Fatal("clearing the manual override did not restore the provider expiry")
			}
		})
	}
}

func TestManualExpiryCancellationAndPersistenceFailurePreservePreviousValue(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	defer m.cancel()
	job, err := m.Submit(Spec{ID: "expiry-write", Email: "expiry-write@example.test", ImportOnly: true, Input: map[string]any{"manualExpiresAt": "2030-01-01T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := m.Get(job.ID)
	value := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.SetManualExpiryContext(ctx, job.ID, &value); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled request changed expiry", err)
	}
	repair := breakAdmissionMetadata(t, st)
	if _, err := m.SetManualExpiryContext(t.Context(), job.ID, &value); !errors.Is(err, ErrAdmissionPersistence) {
		t.Fatal("failed storage did not reject metadata update", err)
	}
	after, _ := m.Get(job.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed metadata update changed the account")
	}
	repair()
	if _, err := m.SetManualExpiryContext(t.Context(), job.ID, &value); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryMetadataCompatibilityAndPublicSanitization(t *testing.T) {
	for _, test := range []struct {
		name         string
		fields       map[string]any
		want, source string
	}{
		{"old-without-expiry", nil, "", "unknown"},
		{"snake-case", map[string]any{"manual_expires_at": "2030-01-02T03:04:00+08:00", "subscription_expires_at": "2035-01-01T00:00:00Z"}, "2030-01-01T19:04:00Z", "manual"},
		{"camel-case", map[string]any{"subscriptionExpiresAt": "2035-01-01T00:00:00Z"}, "2035-01-01T00:00:00Z", "subscription"},
		{"invalid-values", map[string]any{"manualExpiresAt": "synthetic-private-value", "subscriptionExpiresAt": "not-a-date", "expires_at": 2000000000}, "", "unknown"},
		{"legacy-numeric-value", map[string]any{"subscription_expires_at": 2000000000}, "", "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			metadata := map[string]any{"id": "expiry-legacy", "email": "expiry-legacy@example.test", "status": StatusImported}
			for key, value := range test.fields {
				metadata[key] = value
			}
			if err := st.WriteJSON(t.Context(), "job-meta.json", map[string]any{"version": 1, "jobs": []any{metadata}}); err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			defer m.cancel()
			if err := m.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			job, ok := m.Get("expiry-legacy")
			if !ok {
				t.Fatal("old metadata lost its account")
			}
			if expires, source := EffectiveExpiry(job); expires != test.want || source != test.source || len(m.pending) != 0 {
				t.Fatal("old metadata acquired an invented expiry or executable work")
			}
		})
	}
	public := SanitizeJobForPublic(&Job{ManualExpiresAt: "synthetic-private-value", SubscriptionExpiresAt: "not-a-date"})
	if public.ManualExpiresAt != "" || public.SubscriptionExpiresAt != "" {
		t.Fatal("expiry metadata exposed an arbitrary private value")
	}
}

func TestManualExpiryCommitCertaintyKeepsVisibleReplacement(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "not-replaced"
		if uncertain {
			name = "replaced-durability-unknown"
		}
		t.Run(name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			before := &Job{ID: "commit-expiry", Email: "commit-expiry@example.test", ManualExpiresAt: "2030-01-01T00:00:00Z"}
			if err := st.WriteJSON(t.Context(), "job-meta.json", jobMetadata(before)); err != nil {
				t.Fatal(err)
			}
			job := cloneJob(before)
			job.ManualExpiresAt = "2031-01-01T00:00:00Z"
			cause := error(os.ErrPermission)
			want, wantError := before.ManualExpiresAt, ErrAdmissionPersistence
			if uncertain {
				// Store's own regression injects sync failure after a real rename.
				// At this boundary the replacement is already visible; preserve
				// that exact fact while checking the account-state policy.
				if err := st.WriteJSON(t.Context(), "job-meta.json", jobMetadata(job)); err != nil {
					t.Fatal(err)
				}
				cause = fmt.Errorf("%w: %w", store.ErrDurabilityUncertain, os.ErrPermission)
				want, wantError = job.ManualExpiresAt, ErrAdmissionUncertain
			}
			if err := applyManualExpiryCommitResult(job, before, cause); !errors.Is(err, wantError) {
				t.Fatal("metadata commit certainty was misclassified", err)
			}
			var disk map[string]any
			if err := st.ReadJSON(t.Context(), "job-meta.json", &disk); err != nil {
				t.Fatal(err)
			}
			if job.ManualExpiresAt != want || disk["manual_expires_at"] != want {
				t.Fatal("metadata commit result disagrees with the visible file replacement")
			}
		})
	}
}

func TestManualExpirySameValueRetryFlushesUncertainMetadata(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, st, nil)
	defer m.cancel()
	job, err := m.Submit(Spec{ID: "expiry-retry", Email: "expiry-retry@example.test", ImportOnly: true, Input: map[string]any{"manualExpiresAt": "2031-01-01T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	// Model the state after a successful replacement and failed directory
	// sync: both the visible file and memory hold the requested new date.
	m.lastPersistError = fmt.Errorf("%w: %w", store.ErrDurabilityUncertain, os.ErrPermission)
	value := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	repair := breakAdmissionMetadata(t, st)
	if result, err := m.SetManualExpiryContext(t.Context(), job.ID, &value); !errors.Is(err, ErrAdmissionPersistence) || result != nil {
		t.Fatal("same-value retry did not attempt a real metadata write", err)
	}
	current, _ := m.Get(job.ID)
	if current.ManualExpiresAt != job.ManualExpiresAt {
		t.Fatal("failed retry discarded the already visible manual date")
	}
	repair()
	if _, err := m.SetManualExpiryContext(t.Context(), job.ID, &value); err != nil || m.lastPersistError != nil {
		t.Fatal("same-value retry did not confirm a successful metadata flush", err)
	}
	var saved struct{ Jobs []map[string]any }
	if err := st.ReadJSON(t.Context(), "job-meta.json", &saved); err != nil || len(saved.Jobs) != 1 || saved.Jobs[0]["manual_expires_at"] != job.ManualExpiresAt {
		t.Fatal("confirmed retry did not persist the expected manual expiry", err)
	}
}
