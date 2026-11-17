package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestPasswordAddReceiptPreservesConfirmedAndPendingCredentials(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	const candidate = "Synthetic-new-password9!"
	for _, tc := range []struct {
		name, receipt string
		accepted      bool
	}{
		{"explicit-error", `{"error":{"code":"invalid_password","message":"private-receipt-canary"},"continue_url":"$BASE/"}`, false},
		{"error-with-external-page", `{"error":"private-receipt-canary","page":{"type":"external_url"}}`, false},
		{"explicit-failure", `{"success":false,"continue_url":"$BASE/"}`, false},
		{"lost-response", "", false},
		{"legacy-continuation", `{"continue_url":"$BASE/"}`, true},
		{"empty-error", `{"error":"","continue_url":"$BASE/"}`, true},
		{"null-error", `{"error":null,"continue_url":"$BASE/"}`, true},
		{"false-error", `{"error":false,"continue_url":"$BASE/"}`, true},
		{"zero-error", `{"error":0,"continue_url":"$BASE/"}`, true},
		{"legacy-external-page", `{"page":{"type":"external_url"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var additions atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				base := "http://" + r.Host
				switch {
				case r.URL.Path == "/api/auth/signin/openai" && r.URL.Query().Get("post_login_add_password") == "true":
					if cookie, err := r.Cookie("audit-session-token"); err != nil || cookie.Value != sessionAuditCookie {
						t.Error("password enrollment did not retain the authenticated session")
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"url": base + "/reset-password/new-password"})
				case r.URL.Path == "/reset-password/new-password":
					fmt.Fprint(w, "<html>new password</html>")
				case r.URL.Path == "/api/accounts/password/add":
					additions.Add(1)
					var body struct{ Password string }
					if json.NewDecoder(r.Body).Decode(&body) != nil || body.Password != candidate {
						t.Error("password mutation changed the selected candidate")
					}
					if tc.name == "lost-response" {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, strings.ReplaceAll(tc.receipt, "$BASE", base))
				default:
					if !serveFreshLoginFixture(t, w, r, sessionAuditEmail) {
						t.Error("unexpected password fixture endpoint")
						w.WriteHeader(http.StatusNotFound)
					}
				}
			}))
			defer remote.Close()
			cfg := sessionAuditConfig(t.TempDir(), remote.URL)
			vault := credentials.NewMemoryStore()
			manager, _, logger := sessionAuditManager(t, cfg, vault)
			sessionAuditImport(t, cfg, manager)
			if _, err := manager.QueueOperation(sessionAuditJobID, "add-password", map[string]any{"newPassword": candidate}); err != nil {
				t.Fatal(err)
			}
			err := sessionAuditExecute(t, cfg, manager, logger, vault)
			if (err == nil) != tc.accepted || additions.Load() != 1 {
				t.Errorf("password receipt result accepted=%t want=%t mutations=%d", err == nil, tc.accepted, additions.Load())
			}
			if err != nil && strings.Contains(err.Error(), "private-receipt-canary") {
				t.Error("remote receipt text leaked into the operation result")
			}
			saved, err := vault.Load(sessionAuditEmail)
			if err != nil {
				t.Fatal(err)
			}
			job, _ := manager.Get(sessionAuditJobID)
			if tc.accepted {
				if saved.Password != candidate || saved.PendingPassword != "" || job.PasswordAddedAt == nil {
					t.Fatal("confirmed password addition did not complete its existing credential contract")
				}
				return
			}
			if saved.Password != "synthetic-password" || saved.PendingPassword != candidate || job.PasswordAddedAt != nil {
				t.Errorf("unconfirmed addition changed confirmed password=%t, retained pending=%t, published completion=%t", saved.Password != "synthetic-password", saved.PendingPassword == candidate, job.PasswordAddedAt != nil)
			}
			if _, err := os.Stat(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "password-add-result.json")); !errors.Is(err, os.ErrNotExist) {
				t.Error("unconfirmed password addition wrote a success artifact")
			}
			_ = manager.Shutdown(context.Background())
			restored, _, _ := sessionAuditManager(t, cfg, vault)
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, ok := restored.Get(sessionAuditJobID)
			saved, err = vault.Load(sessionAuditEmail)
			if err != nil || !ok || saved.Password != "synthetic-password" || saved.PendingPassword != candidate || current.PasswordAddedAt != nil || current.Status == "queued" || additions.Load() != 1 {
				t.Error("restart lost pending material, confirmed the failed addition, or replayed the remote mutation")
			}
		})
	}
}
