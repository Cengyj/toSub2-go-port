package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestBatchAPIImportsWithoutStartingAndRequiresExplicitSelection(t *testing.T) {
	for _, action := range []string{"retry", "relogin", "reauthorize-batch", "setup-2fa", "add-password"} {
		t.Run(action, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			m.SetCredentialStore(credentials.NewMemoryStore())
			bootstrap := frontendRequest(s, http.MethodGet, "/api/bootstrap", nil, "test")
			var advertised struct {
				Features map[string]bool `json:"features"`
			}
			// Features also includes the SMS provider list, so read only the
			// capability needed by this client's import workflow.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(bootstrap.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			var features map[string]json.RawMessage
			if err := json.Unmarshal(raw["features"], &features); err != nil {
				t.Fatal(err)
			}
			if string(features["batchImportOnly"]) != "true" {
				t.Fatal("import-only capability not advertised", advertised)
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "manual@example.test"}, "test")
			if w.Code != http.StatusCreated {
				t.Fatal(w.Code, w.Body.String())
			}
			j := m.List()[0]
			if j.Status != jobs.StatusImported || j.Attempts != 0 || m.QueuePosition(j.ID) != 0 {
				t.Fatal("batch import entered the queue")
			}
			flags := s.publicJob(j)
			for _, flag := range []string{"canRetry", "canForceRelogin", "canSetupTotp", "canAddPassword"} {
				if flags[flag] != true {
					t.Fatal("imported account cannot choose", flag)
				}
			}
			if flags["autoRepairEligible"] != false || flags["canDownload"] != false || flags["lastOperationType"] != "account_import" {
				t.Fatal("import was reported as completed authorization")
			}
			path, body := "/api/jobs/"+j.ID+"/"+action, map[string]any{}
			if action == "reauthorize-batch" {
				path, body = "/api/jobs/reauthorize-batch", map[string]any{"ids": []string{j.ID}}
			}
			w = frontendRequest(s, http.MethodPost, path, body, "test")
			if w.Code != http.StatusOK {
				t.Fatal("explicit operation rejected", w.Code, w.Body.String())
			}
			got, _ := m.Get(j.ID)
			if got.Status != jobs.StatusQueued || m.QueuePosition(j.ID) == 0 {
				t.Fatal("explicit operation did not schedule account")
			}
			if action == "setup-2fa" || action == "add-password" {
				if got.Operation != action || got.SecurityActionReturnStatus != jobs.StatusImported {
					t.Fatal("security selection became authorization")
				}
			} else if got.LastOperationType != "initial_authorization" {
				t.Fatal("first authorization labeled as a retry")
			}
		})
	}
}

func TestBatchReimportRetainsProxyAndCountsUnchangedAccounts(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	w := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "existing@example.test----Original-password9!", "proxyUrl": "http://proxy.example:8080"}, "test")
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code)
	}
	j := m.List()[0]
	// A bare-email import preserves its account-specific mailbox even when the
	// browser currently has a different global mail configuration.
	m.Update(j.ID, map[string]any{"mailApiUrl": "https://original-mail.example/messages"})
	frontendRequest(s, http.MethodPost, "/api/mail-request-config", map[string]any{"config": map[string]any{"url": "https://new-mail.example/messages", "method": "GET"}}, "test")
	w = frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "EXISTING@example.test\nnew@example.test"}, "test")
	var result struct {
		Created int `json:"created"`
		Updated int `json:"updated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusCreated || result.Created != 1 || result.Updated != 1 {
		t.Fatal("existing-account count differs from the original console", w.Code, result)
	}
	got, _ := m.Get(j.ID)
	if got.Input["proxyUrl"] != "http://proxy.example:8080" || got.Input["password"] != "Original-password9!" || got.MailAPIURL != "https://original-mail.example/messages" {
		t.Fatal("bare reimport discarded account credentials")
	}
	w = frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/retry", map[string]any{}, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	w = frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"text": "existing@example.test----Replacement-password9!"}, "test")
	if w.Code != http.StatusConflict {
		t.Fatal("queued account credentials changed through import", w.Code)
	}
	got, _ = m.Get(j.ID)
	if got.Input["password"] != "Original-password9!" || got.Status != jobs.StatusQueued {
		t.Fatal("rejected import changed queued job")
	}
	if strings.Contains(w.Body.String(), "Replacement-password9!") {
		t.Fatal("conflict response revealed supplied password")
	}
}
