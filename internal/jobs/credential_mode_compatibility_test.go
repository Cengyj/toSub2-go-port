package jobs

import (
	"context"
	"encoding/json"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestProtectedCredentialUpdatesPreserveLegacyLoginMode(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, err := m.Submit(Spec{Email: "credential-mode@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, password, mode string
	}{
		{"add-password", "Synthetic-password9!", "password"},
		{"return-to-email-otp", "", "email_otp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.UpdateStoredCredentials(j.ID, credentials.Credentials{Password: tc.password})
			current, _ := m.Get(j.ID)
			if current.LoginMode != tc.mode || current.HasPassword != (tc.password != "") {
				t.Fatalf("protected credentials and login mode disagree: mode=%q hasPassword=%v", current.LoginMode, current.HasPassword)
			}
		})
	}
}

func TestSuccessfulPasswordSetupPersistsPasswordLoginMode(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), 1, st, nil)
	j, err := m.Submit(Spec{Email: "password-added@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	m.UpdatePasswordAdded(j.ID, true, "")
	current, _ := m.Get(j.ID)
	if current.LoginMode != "password" || !current.HasPassword || current.PasswordAddedAt == nil {
		t.Fatalf("password setup left stale account login state: mode=%q hasPassword=%v addedAt=%v", current.LoginMode, current.HasPassword, current.PasswordAddedAt)
	}
	var metadata map[string]json.RawMessage
	if err := st.ReadJSON(context.Background(), "job-meta.json", &metadata); err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	if err := json.Unmarshal(metadata["jobs"], &records); err != nil || len(records) != 1 {
		t.Fatal("missing persisted job", err)
	}
	if records[0]["login_mode"] != "password" || records[0]["has_password"] != true {
		t.Fatal("persisted login state did not reflect successful password setup")
	}
}
