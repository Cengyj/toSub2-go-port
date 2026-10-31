package sub2api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadExplicitUnknownSubscriptionClearsRemoteExpiry(t *testing.T) {
	for _, source := range []string{"credentials", "extra"} {
		t.Run(source, func(t *testing.T) {
			writes := 0
			remote := uploadFixtureAccount("owner@example.test", "synthetic-old-access")
			remote["id"] = 12
			for _, parent := range []string{"credentials", "extra"} {
				remote[parent].(map[string]any)["subscription_expires_at"] = "2030-01-01T00:00:00Z"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
					writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{remote}, "pages": 1}})
				case r.Method == http.MethodGet:
					writeUploadFixture(t, w, map[string]any{"data": remote})
				case r.Method == http.MethodPut:
					writes++
					var payload map[string]map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
						return
					}
					for _, parent := range []string{"credentials", "extra"} {
						if _, exists := payload[parent]["subscription_expires_at"]; exists {
							t.Error("explicit unknown subscription retained an old expiry")
						}
					}
					fmt.Fprint(w, `{"ok":true}`)
				default:
					t.Error("unexpected mutation")
				}
			}))
			defer server.Close()
			local := uploadFixtureAccount("owner@example.test", "synthetic-new-access")
			local[source].(map[string]any)["subscription_expires_at"] = nil
			_, err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin"}).Upload(t.Context(), uploadFixturePayload(local))
			if err != nil || writes != 1 {
				t.Fatalf("writes=%d error=%v", writes, err)
			}
		})
	}
}

func TestMutationNumberDecodingRejectsTrailingJSON(t *testing.T) {
	for _, body := range []string{`{"ok":true} {"success":false}`, `{"ok":true} trailing-synthetic-value`} {
		t.Run(body, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writes++
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin"}).Update(t.Context(), "12", map[string]any{"status": "active"})
			if !errors.Is(err, ErrMutationOutcomeUnknown) || writes != 1 || strings.Contains(err.Error(), "trailing-synthetic") {
				t.Fatalf("trailing JSON accepted, replayed or leaked: writes=%d error=%v", writes, err)
			}
		})
	}
}

func TestPreservedJSONNumbersKeepExactIntegerCompatibility(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  int64
		valid bool
	}{
		{"1", 1, true}, {"1.0", 1, true}, {"1e2", 100, true},
		{"0", 0, true}, {"-0", 0, true}, {"0.0", 0, true}, {"0e2", 0, true},
		{"9007199254740991", 9007199254740991, true},
		{"1.00000000000000001", 0, false}, {"9007199254740990.1", 0, false},
		{"9007199254740993", 0, false}, {"1e1000000", 0, false}, {"1e-1000000", 0, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, valid := nonnegativeInteger(json.Number(tc.raw))
			if got != tc.want || valid != tc.valid {
				t.Fatalf("integer=(%d,%t) want=(%d,%t)", got, valid, tc.want, tc.valid)
			}
		})
	}
}

func TestAuthorizationUpdatePreservesUnknownJSONNumbers(t *testing.T) {
	for _, operation := range []string{"upload", "monitor"} {
		t.Run(operation, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
					fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"pages":1}}`)
				case r.Method == http.MethodGet:
					fmt.Fprint(w, `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test","provider_integer":9007199254740993},"extra":{"provider_decimal":1.234567890123456789,"nested":{"integer":9223372036854775807}}}}`)
				case r.Method == http.MethodPut:
					writes++
					var payload struct {
						Credentials map[string]json.RawMessage `json:"credentials"`
						Extra       map[string]json.RawMessage `json:"extra"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
						return
					}
					if string(payload.Credentials["provider_integer"]) != "9007199254740993" || string(payload.Extra["provider_decimal"]) != "1.234567890123456789" || !strings.Contains(string(payload.Extra["nested"]), "9223372036854775807") {
						t.Error("authorization update rounded unrelated remote configuration")
					}
					fmt.Fprint(w, `{"ok":true}`)
				default:
					fmt.Fprint(w, `{"ok":true}`)
				}
			}))
			defer server.Close()
			client := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin"})
			local := uploadFixtureAccount("owner@example.test", "synthetic-access")
			var err error
			if operation == "upload" {
				_, err = client.Upload(t.Context(), uploadFixturePayload(local))
			} else {
				err = client.SyncAccountCredentials(t.Context(), 12, local["credentials"].(map[string]any), "session")
			}
			if err != nil || writes != 1 {
				t.Fatalf("writes=%d error=%v", writes, err)
			}
		})
	}
}

func TestUploadCreationPreservesUnknownJSONNumbers(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"data":{"items":[],"pages":0,"total":0}}`)
			return
		}
		writes++
		var payload struct {
			Accounts []map[string]json.RawMessage `json:"accounts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if len(payload.Accounts) != 1 || string(payload.Accounts[0]["provider_integer"]) != "9007199254740993" {
			t.Error("creation rounded an unknown import field")
		}
		fmt.Fprint(w, `{"created":1}`)
	}))
	defer server.Close()
	local := uploadFixtureAccount("owner@example.test", "synthetic-access")
	local["provider_integer"] = json.Number("9007199254740993")
	_, err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin"}).Upload(t.Context(), uploadFixturePayload(local))
	if err != nil || writes != 1 {
		t.Fatalf("writes=%d error=%v", writes, err)
	}
}

func TestMonitorSyncPreservesExplicitRemoteAdministrativeState(t *testing.T) {
	for _, status := range []string{"active", "disabled", "error", ""} {
		t.Run(status, func(t *testing.T) {
			updates, lifecycle := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					fmt.Fprintf(w, `{"data":{"id":12,"status":%q,"schedulable":false,"credentials":{"email":"owner@example.test"}}}`, status)
				case http.MethodPut:
					updates++
					fmt.Fprint(w, `{"ok":true}`)
				default:
					lifecycle++
					fmt.Fprint(w, `{"ok":true}`)
				}
			}))
			defer server.Close()
			err := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin"}).SyncAccountCredentials(t.Context(), 12, map[string]any{"email": "owner@example.test", "access_token": "synthetic-access"}, "session")
			wantLifecycle := 0
			if status == "error" || status == "" {
				wantLifecycle = 2
			}
			if err != nil || updates != 1 || lifecycle != wantLifecycle {
				t.Fatalf("status=%q updates=%d lifecycle=%d want=%d error=%v", status, updates, lifecycle, wantLifecycle, err)
			}
		})
	}
}
