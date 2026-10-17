package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestUploadNestedDetailRejectionStopsBeforeMutation(t *testing.T) {
	for _, rejection := range []string{`"ok":false`, `"success":false`} {
		t.Run(rejection, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method != http.MethodGet:
					writes.Add(1)
					_, _ = fmt.Fprint(w, `{"ok":true}`)
				case r.URL.Path == "/api/v1/admin/accounts":
					_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"pages":1}}`)
				default:
					_, _ = fmt.Fprintf(w, `{"ok":true,"data":{%s,"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}}`, rejection)
				}
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "synthetic-access")))
			if err == nil || IsMutationOutcomeUnknown(err) || writes.Load() != 0 {
				t.Fatalf("rejected account detail authorized a write: writes=%d error=%v", writes.Load(), err)
			}
		})
	}
}

func TestUploadCreationConflictingIdentityEvidenceIsUnknown(t *testing.T) {
	for index, value := range []any{
		map[string]any{"created": 2, "ids": []any{1, 2}, "account_ids": []any{3, 4}},
		map[string]any{"created": 2, "ids": []any{1, 2}, "data": map[string]any{"ids": []any{3, 4}}},
		map[string]any{"created": 2, "ids": "invalid"},
	} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			got, err := uploadCreationResult(value, 2)
			if !errors.Is(err, ErrMutationOutcomeUnknown) || got.unknown != 2 || got.created != 0 || len(got.ids) != 0 {
				t.Fatalf("conflicting identity evidence reported success: outcome=%+v error=%v", got, err)
			}
		})
	}
}

func TestUploadCreationMatchingIdentityAliasesRemainCompatible(t *testing.T) {
	for _, value := range []any{
		map[string]any{"created": 2, "ids": []any{2, 1}, "account_ids": []any{1, 2}},
		map[string]any{"created": 2, "ids": []any{1, 2}, "data": map[string]any{"ids": []any{2, 1}}},
		map[string]any{"created": 2, "ids": nil},
	} {
		got, err := uploadCreationResult(value, 2)
		if err != nil || got.created != 2 || got.unknown != 0 {
			t.Fatalf("consistent optional identity metadata was rejected: outcome=%+v error=%v", got, err)
		}
	}
}

func TestCredentialSyncRequiresAcceptedCompleteRemoteCredentials(t *testing.T) {
	for _, body := range []string{
		`{"ok":false,"data":{"id":12,"credentials":{"email":"owner@example.test"}}}`,
		`{"data":{"success":false,"id":12,"credentials":{"email":"owner@example.test"}}}`,
		`{"data":{"id":12}}`,
		`{"data":{"id":12,"credentials":null}}`,
		`{"data":{"id":12,"credentials":"redacted"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, body)
					return
				}
				writes.Add(1)
				_, _ = fmt.Fprint(w, `{"ok":true}`)
			}))
			defer server.Close()
			err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin-key"}).SyncAccountCredentials(t.Context(), 12, map[string]any{"email": "owner@example.test", "access_token": "synthetic-access"}, "session")
			if err == nil || writes.Load() != 0 {
				t.Fatalf("unavailable remote settings authorized destructive replacement: writes=%d error=%v", writes.Load(), err)
			}
		})
	}
}

func TestMonitorIncompleteLookupCannotAuthorizeExcludedPlanRepair(t *testing.T) {
	var repairs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","status":"error","credentials":{"email":"owner@example.test","plan_type":"plus"}}],"page":1,"pages":2,"total":2}}`)
			return
		}
		// The missing page could hold a healthy excluded-plan record for the
		// same identity; an incomplete lookup cannot authorize relogin.
		_, _ = fmt.Fprint(w, `{"ok":false,"data":{"items":[],"page":2,"pages":2,"total":2}}`)
	}))
	defer server.Close()
	monitor := NewMonitorService(New(Config{BaseURL: server.URL, APIKey: "synthetic-admin-key"}), MonitorConfig{Enabled: true, ExcludedPlanTypes: []string{"pro"}}, MonitorHooks{
		Evaluate: func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
			return RepairDecision{Action: ActionStart}, nil
		},
		StartRepair: func(context.Context, string, []RemoteAccount) error {
			repairs.Add(1)
			return nil
		},
	}, "")
	summary, err := monitor.Check(t.Context(), "synthetic-boundary-test")
	if err == nil || summary.Error == "" || summary.Started != 0 || repairs.Load() != 0 {
		t.Fatalf("incomplete lookup authorized a repair: started=%d repairs=%d error=%v", summary.Started, repairs.Load(), err)
	}
}

func TestMonitorAccountListCompatibilityAndCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"bare-list", `[{"id":"12"}]`, true},
		{"enveloped-list", `{"data":[{"id":12}]}`, true},
		{"outer-pagination", `{"data":[{"id":12}],"page":"1","pages":"1","total":"1"}`, true},
		{"empty-list", `{"data":{"items":[],"pages":0,"total":0}}`, true},
		{"missing-items", `{"data":{"pages":1}}`, false},
		{"missing-total", `{"data":{"items":[{"id":12}],"pages":1,"total":2}}`, false},
		{"wrong-page", `{"data":{"items":[],"page":2,"pages":2}}`, false},
		{"fractional-pages", `{"data":{"items":[],"pages":1.5}}`, false},
		{"too-many-pages", `{"data":{"items":[],"pages":1001}}`, false},
		{"duplicate-identities", `{"data":{"items":[{"id":12},{"id":12}],"pages":1}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			got, err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin-key"}).OpenAIAccounts(t.Context())
			if (err == nil) != tc.ok || !tc.ok && len(got) != 0 {
				t.Fatalf("lookup completeness: accepted=%t want=%t returned=%d", err == nil, tc.ok, len(got))
			}
		})
	}
}

func FuzzAccountListPage(f *testing.F) {
	for _, seed := range []string{`[]`, `null`, `{"data":{"items":[],"total":0}}`, `{"ok":false,"data":[]}`, `{"pages":1,"data":{"pages":2,"items":[]}}`, `{"items":[],"pages":1.5}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		var value any
		if json.Unmarshal([]byte(input), &value) != nil {
			return
		}
		items, metadata, err := accountListPage(value)
		if err == nil {
			if items == nil {
				t.Fatal("missing items were accepted as a complete page")
			}
			for _, count := range metadata {
				if count < 0 || count > 1<<53-1 {
					t.Fatal("pagination accepted an unsafe integer")
				}
			}
		}
		for _, key := range []string{"ok", "success"} {
			if _, _, err := accountListPage(map[string]any{key: false, "data": value}); err == nil {
				t.Fatal("business refusal accepted as a complete page")
			}
		}
	})
}
