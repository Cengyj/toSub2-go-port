package sub2api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"tosub2-go-port/internal/protocol"
)

func TestAuthorizationWorkspaceChangeDoesNotRetainAnotherRefreshGrant(t *testing.T) {
	for _, operation := range []string{"upload", "monitor"} {
		for _, clientID := range []string{protocol.DefaultCodexClientID, protocol.DefaultBPSClientID} {
			for _, tc := range []struct {
				name, workspace, incomingRefresh, wantRefresh string
			}{
				{"new-workspace-without-refresh", "new-workspace", "", ""},
				{"new-workspace-with-refresh", "new-workspace", "synthetic-new-refresh", "synthetic-new-refresh"},
				{"same-workspace-without-refresh", "old-workspace", "", "synthetic-old-refresh"},
			} {
				t.Run(operation+"/"+clientID+"/"+tc.name, func(t *testing.T) {
					remote := uploadFixtureAccount("owner@example.test", "synthetic-old-access")
					remote["id"], remote["status"] = 12, "active"
					remoteCredentials := remote["credentials"].(map[string]any)
					remoteCredentials["client_id"], remoteCredentials["chatgpt_account_id"] = clientID, "old-workspace"
					remoteCredentials["refresh_token"] = "synthetic-old-refresh"
					remoteCredentials["provider_setting"] = "preserved"
					writes := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
							writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{remote}, "pages": 1}})
						case r.Method == http.MethodGet:
							writeUploadFixture(t, w, map[string]any{"data": remote})
						case r.Method == http.MethodPut:
							writes++
							var payload map[string]map[string]any
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								t.Error(err)
								return
							}
							credentials := payload["credentials"]
							if stringValue(credentials["refresh_token"]) != tc.wantRefresh {
								t.Error("new authorization retained another workspace's refresh grant or lost a compatible grant")
							}
							if credentials["access_token"] != "synthetic-new-access" || credentials["chatgpt_account_id"] != tc.workspace || credentials["client_id"] != clientID || credentials["provider_setting"] != "preserved" {
								t.Error("grant identity or unrelated provider setting was lost")
							}
							writeUploadFixture(t, w, map[string]any{"ok": true})
						default:
							t.Error("unexpected mutation")
						}
					}))
					defer server.Close()
					local := uploadFixtureAccount("owner@example.test", "synthetic-new-access")
					incoming := local["credentials"].(map[string]any)
					incoming["client_id"], incoming["chatgpt_account_id"] = clientID, tc.workspace
					delete(incoming, "refresh_token")
					if tc.incomingRefresh != "" {
						incoming["refresh_token"] = tc.incomingRefresh
					}
					client := New(Config{BaseURL: server.URL, APIKey: "synthetic-admin"})
					var err error
					if operation == "upload" {
						_, err = client.Upload(t.Context(), uploadFixturePayload(local))
					} else {
						err = client.SyncAccountCredentials(t.Context(), 12, incoming, "session")
					}
					if err != nil || writes != 1 {
						t.Fatalf("writes=%d error=%v", writes, err)
					}
				})
			}
		}
	}
}
