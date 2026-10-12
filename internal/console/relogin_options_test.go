package console

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestReloginUploadAPIValidatesBeforeMutationAndKeepsKeyPrivate(t *testing.T) {
	for _, batch := range []bool{false, true} {
		mode := "single"
		if batch {
			mode = "batch"
		}
		for _, scenario := range []string{"null", "string", "bad-config", "success", "disabled"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				m.SetCredentialStore(credentials.NewMemoryStore())
				j := importTOTPActionFixture(t, m, "relogin-upload", true)
				body := map[string]any{"uploadAfterRelogin": true, "sub2apiConfig": map[string]any{"baseUrl": "https://sub2api.example.test", "adminApiKey": "relogin-api-key-canary"}}
				switch scenario {
				case "null":
					body["uploadAfterRelogin"] = nil
				case "string":
					body["uploadAfterRelogin"] = "true"
				case "bad-config":
					body["sub2apiConfig"] = map[string]any{"baseUrl": "invalid", "adminApiKey": "relogin-api-key-canary"}
				case "disabled":
					body["uploadAfterRelogin"] = false
				}
				path := "/api/jobs/" + j.ID + "/relogin"
				if batch {
					path, body["ids"] = "/api/jobs/relogin-batch", []string{j.ID}
				}
				w := frontendRequest(s, http.MethodPost, path, body, "test")
				state, _ := m.Get(j.ID)
				valid := scenario == "success" || scenario == "disabled"
				if !valid {
					if w.Code != http.StatusBadRequest || state.Status != jobs.StatusImported || m.QueuePosition(j.ID) != 0 {
						t.Fatal("invalid auto-upload request changed the account", w.Code)
					}
					return
				}
				if w.Code != http.StatusOK || state.Status != jobs.StatusQueued || state.UploadAfterRelogin != (scenario == "success") {
					t.Fatal("relogin options not preserved", w.Code, state)
				}
				var result map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				var public map[string]any
				if batch {
					public = result["jobs"].([]any)[0].(map[string]any)
				} else {
					public = result["job"].(map[string]any)
				}
				if public["uploadAfterRelogin"] != (scenario == "success") || public["reloginAuthorizationSaved"] != false {
					t.Fatal("public relogin state omitted or confused old/new grant")
				}
				values, _ := m.InputCopy(j.ID)
				if (values["sub2apiConfig"] != nil) != (scenario == "success") {
					t.Fatal("enabled upload lost config or disabled upload retained key")
				}
				meta, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
				if err != nil || strings.Contains(string(meta), "relogin-api-key-canary") || strings.Contains(w.Body.String(), "relogin-api-key-canary") {
					t.Fatal("integration key exposed in API or metadata", err)
				}
			})
		}
	}
}

func TestReloginUploadOptionsCannotEnableSyncForOtherActions(t *testing.T) {
	for _, action := range []string{"regenerate", "setup-2fa", "delete", "reauthorize", "add-password"} {
		if _, err := normalizeTOTPAction(action, map[string]any{"uploadAfterRelogin": true}); err == nil {
			t.Fatal("auto-upload attached to unrelated action", action)
		}
	}
}
