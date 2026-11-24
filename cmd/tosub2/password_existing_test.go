package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestPasswordAdditionSkipsFreshlyVerifiedExistingPassword(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var additions, reauth atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/accounts/password/add" {
			additions.Add(1)
		}
		if r.URL.Query().Get("post_login_add_password") == "true" {
			reauth.Add(1)
		}
		if !serveFreshLoginFixture(t, w, r, sessionAuditEmail) {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer remote.Close()
	cfg := sessionAuditConfig(t.TempDir(), remote.URL)
	vault := credentials.NewMemoryStore()
	manager, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditImport(t, cfg, manager)
	if _, err := manager.QueueOperation(sessionAuditJobID, "add-password", map[string]any{"newPassword": "Unused-candidate-password9!"}); err != nil {
		t.Fatal(err)
	}
	if err := sessionAuditExecute(t, cfg, manager, logger, vault); err != nil {
		t.Fatal(err)
	}
	saved, err := vault.Load(sessionAuditEmail)
	job, ok := manager.Get(sessionAuditJobID)
	if err != nil || !ok || saved.Password != "synthetic-password" || saved.PendingPassword != "" || !job.HasPassword || job.PasswordAddedAt != nil || job.PasswordAddOutcome != "already_exists" || job.OperationStatus != "succeeded" || additions.Load() != 0 || reauth.Load() != 0 {
		t.Fatal("existing password was replaced or reported as newly added", err, additions.Load(), reauth.Load())
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputRoot, sessionAuditJobID, "password-add-result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("skipped password addition created success material", err)
	}
}
