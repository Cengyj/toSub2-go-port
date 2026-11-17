package sub2api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUploadPreservesDeepRemoteProviderMetadata(t *testing.T) {
	// Remote provider metadata is opaque to authorization updates. A duplicate
	// field guard must not impose a new nesting limit on this valid JSON.
	metadata := strings.Repeat(`{"nested":`, 80) + `"synthetic-value"` + strings.Repeat(`}`, 80)
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"total":1}}`)
		case r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"},"extra":{"provider_metadata":%s}}}`, metadata)
		case r.Method == http.MethodPut:
			writes.Add(1)
			var payload struct {
				Extra map[string]json.RawMessage `json:"extra"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || string(payload.Extra["provider_metadata"]) != metadata {
				t.Error("authorization update changed opaque provider metadata")
			}
			_, _ = fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "synthetic-access")))
	if err != nil || writes.Load() != 1 {
		t.Fatalf("valid provider metadata blocked authorization update: writes=%d error=%v", writes.Load(), err)
	}
}

func TestUploadRejectsAmbiguousLookupBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name, list, detail string
	}{
		{
			name: "list-overwrites-existing-account",
			list: `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"total":1},"data":{"items":[],"total":0}}`,
		},
		{
			name:   "detail-overwrites-identity",
			list:   `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"total":1}}`,
			detail: `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"different@example.test","email":"owner@example.test"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
					_, _ = fmt.Fprint(w, `{"ok":true}`)
					return
				}
				if r.URL.Path == "/api/v1/admin/accounts" {
					_, _ = fmt.Fprint(w, tc.list)
					return
				}
				_, _ = fmt.Fprint(w, tc.detail)
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "synthetic-access")))
			if err == nil || IsMutationOutcomeUnknown(err) || writes.Load() != 0 {
				t.Fatalf("ambiguous lookup authorized a write: writes=%d error=%v", writes.Load(), err)
			}
		})
	}
}

func TestUploadAmbiguousMutationReceiptRemainsUnknown(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
					_, _ = fmt.Fprint(w, `{"success":false,"success":true}`)
					return
				}
				if r.URL.Path == "/api/v1/admin/accounts" {
					if existing {
						_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"total":1}}`)
					} else {
						_, _ = fmt.Fprint(w, `{"data":{"items":[],"total":0}}`)
					}
					return
				}
				_, _ = fmt.Fprint(w, `{"data":{"id":12,"platform":"openai","type":"oauth","status":"error","credentials":{"email":"owner@example.test"}}}`)
			}))
			defer server.Close()
			result, err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin-key"}).Upload(t.Context(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "synthetic-access")))
			public := PublicUploadResult(result)
			if err == nil || !IsMutationOutcomeUnknown(err) || writes.Load() != 1 || public["success"] != float64(0) || public["unknown"] != float64(1) {
				t.Fatalf("ambiguous receipt confirmed or repeated a write: writes=%d result=%v error=%v", writes.Load(), public, err)
			}
		})
	}
}
