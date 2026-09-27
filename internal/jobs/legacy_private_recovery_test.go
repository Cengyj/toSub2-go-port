package jobs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/mail"
	"tosub2-go-port/internal/store"
)

func TestLegacyMailboxMetadataRestoresAutomaticHTTPInput(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy_only", true: "protected_record_wins"}[newer], func(t *testing.T) {
			wantBody := "{\"mailbox\":\"synthetic-mailbox-key\"}"
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPost || string(body) != wantBody || r.URL.Query().Get("token") != "synthetic-api-key" {
					t.Error("restored mailbox request lost its URL/method/body")
					w.WriteHeader(400)
					return
				}
				_, _ = w.Write([]byte("{\"verification_code\":\"582947\"}"))
			}))
			defer remote.Close()
			root, _ := store.New(t.TempDir())
			dir, _ := store.New(filepath.Join(root.Root, "mailbox"))
			url := remote.URL + "/messages?token=synthetic-api-key"
			legacyURL, legacyBody := url, wantBody
			if newer {
				legacyURL = "https://stale.example.test/old-token"
				legacyBody = "old-body"
			}
			if err := dir.WriteJSON(context.Background(), "job-meta.json", map[string]any{"email": "mailbox@example.test", "status": "queued", "mail_api_url": legacyURL, "mail_request_body": legacyBody}); err != nil {
				t.Fatal(err)
			}
			vault := credentials.NewMemoryStore()
			if newer {
				if err := vault.Save("mailbox@example.test", credentials.Credentials{Password: "Preserved-Password9!", MailAPIURL: url, MailMethod: "POST", MailBody: wantBody}); err != nil {
					t.Fatal(err)
				}
			}
			m := NewManager(context.Background(), 1, root, nil)
			m.SetCredentialStore(vault)
			if err := m.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			j, _ := m.Get("mailbox")
			if j.MailAPIURL != url {
				t.Fatal("legacy migration lost or downgraded automatic mailbox URL")
			}
			c, err := vault.Load(j.Email)
			if err != nil {
				t.Fatal(err)
			}
			if c.MailAPIURL != url || c.MailBody != wantBody || c.MailMethod != "POST" {
				t.Fatal("mailbox input was not moved to credential storage")
			}
			if newer && c.Password != "Preserved-Password9!" {
				t.Fatal("migration replaced newer protected credentials")
			}
			in, _ := m.InputCopy(j.ID)
			candidates, err := mail.FetchCandidates(context.Background(), in["mailApiUrl"].(string), mail.RequestConfig{Method: in["mailRequestMethod"].(string), Body: in["mailRequestBody"].(string)}, remote.Client())
			if err != nil || len(candidates) != 1 || candidates[0].Code != "582947" {
				t.Fatal("restored mailbox cannot deliver OTP", err)
			}
			meta, err := os.ReadFile(root.Path("job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"synthetic-api-key", "synthetic-mailbox-key", "Preserved-Password9!", "old-token"} {
				if strings.Contains(string(meta), secret) {
					t.Fatal("mailbox or account secret written to Go metadata")
				}
			}
			restarted := NewManager(context.Background(), 1, root, nil)
			restarted.SetCredentialStore(vault)
			if err := restarted.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			if j, _ := restarted.Get("mailbox"); j.MailAPIURL != url {
				t.Fatal("mailbox migration was not restart-compatible")
			}
		})
	}
}

type failedRecoverySave struct{}

func (*failedRecoverySave) Load(string) (credentials.Credentials, error) {
	return credentials.Credentials{}, os.ErrNotExist
}
func (*failedRecoverySave) Save(string, credentials.Credentials) error {
	return errors.New("password=Synthetic-Do-Not-Expose")
}
func (*failedRecoverySave) Delete(string) error { return nil }

func TestPrivateSettingRecoveryFailureRetainsOriginalOutputAndSafeError(t *testing.T) {
	for _, op := range []string{"password", "totp"} {
		t.Run(op, func(t *testing.T) {
			root, _ := store.New(t.TempDir())
			dir, _ := store.New(filepath.Join(root.Root, "pending"))
			write := func(name string, v any) {
				t.Helper()
				if err := dir.WriteJSON(context.Background(), name, v); err != nil {
					t.Fatal(err)
				}
			}
			status := "totp_starting"
			file := "totp-setup-result.json"
			value := map[string]any{"version": 1, "email": "owner@example.test", "secret": "JBSWY3DPEHPK3PXP", "activation_succeeded": true}
			if op == "password" {
				status = "password_add_starting"
				file = "password-add-result.json"
				value = map[string]any{"version": 1, "email": "owner@example.test", "password": "Synthetic-Do-Not-Expose"}
			}
			write("job-meta.json", map[string]any{"email": "owner@example.test", "status": status})
			write("sub2api-import-oauth.json", map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": "owner@example.test"}}}})
			write(file, value)
			m := NewManager(context.Background(), 1, root, nil)
			m.SetCredentialStore(&failedRecoverySave{})
			if err := m.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			j, _ := m.Get("pending")
			if j.Status != StatusSucceeded || !j.ResultSaved || len(m.pending) != 0 {
				t.Fatal("failed secure recovery replays mutation or hides old authorization")
			}
			if (op == "password" && j.PasswordAddError == "") || (op == "totp" && j.TOTPSetupError == "") {
				t.Fatal("credential failure was not visible without a logger")
			}
			if _, err := os.Stat(dir.Path(file)); err != nil {
				t.Fatal("failed recovery deleted private recovery file")
			}
			meta, _ := os.ReadFile(root.Path("job-meta.json"))
			if strings.Contains(string(meta), "Synthetic-Do-Not-Expose") {
				t.Fatal("raw vault error leaked")
			}
		})
	}
}

func TestLegacyCredentialRequirementFlagsAndForeignArtifacts(t *testing.T) {
	for _, login := range []string{"password", "email_otp"} {
		root, _ := store.New(t.TempDir())
		dir, _ := store.New(filepath.Join(root.Root, "old"))
		if err := dir.WriteJSON(context.Background(), "job-meta.json", map[string]any{"email": "owner@example.test", "status": "queued", "login_mode": login, "has_stored_credentials": true}); err != nil {
			t.Fatal(err)
		}
		m := NewManager(context.Background(), 1, root, nil)
		if err := m.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		if j, _ := m.Get("old"); j.Status != "reauth_required" {
			t.Fatal("legacy combined credential flag permitted automatic identity change")
		}
	}
	root, _ := store.New(t.TempDir())
	dir, _ := store.New(filepath.Join(root.Root, "mixed"))
	for name, value := range map[string]any{
		"job-meta.json":             map[string]any{"email": "owner@example.test", "status": "phone_otp"},
		"sub2api-import-oauth.json": map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": "unrelated@example.test"}}}},
		"login-checkpoint.json":     map[string]any{"version": 1, "email": "owner@example.test", "oauth": map[string]any{"phone": "+12025550171"}},
	} {
		if err := dir.WriteJSON(context.Background(), name, value); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(context.Background(), 1, root, nil)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j, _ := m.Get("mixed"); j.Status != "resume_available" || j.ResultSaved || j.CurrentPhone != "+12025550171" {
		t.Fatal("unrelated output masked or contaminated matching phone checkpoint")
	}
}

func TestLegacyMailboxBackfillsEarlierAggregateWithoutReplacingPublicState(t *testing.T) {
	st, _ := store.New(t.TempDir())
	dir, _ := store.New(filepath.Join(st.Root, "upgraded"))
	if err := dir.WriteJSON(context.Background(), "job-meta.json", map[string]any{"email": "upgrade@example.test", "status": "queued", "mail_api_url": "https://mail.example.test/api?token=synthetic-legacy-key"}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteJSON(context.Background(), "job-meta.json", map[string]any{"version": 1, "jobs": []any{map[string]any{"id": "upgraded", "email": "upgrade@example.test", "status": "canceled", "last_error": "newer safe state"}}}); err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	m := NewManager(context.Background(), 1, st, nil)
	m.SetCredentialStore(vault)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, _ := m.Get("upgraded")
	if j.Status != StatusCanceled || j.Error != "newer safe state" || j.MailAPIURL == "" {
		t.Fatal("backfill changed newer public state or lost mailbox")
	}
	c, err := vault.Load(j.Email)
	if err != nil || c.MailAPIURL != j.MailAPIURL {
		t.Fatal("backfill not stored securely", err)
	}
	data, _ := os.ReadFile(st.Path("job-meta.json"))
	if strings.Contains(string(data), "synthetic-legacy-key") {
		t.Fatal("backfill copied secret into public aggregate")
	}
}
