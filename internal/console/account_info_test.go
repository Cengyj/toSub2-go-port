package console

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestAccountInfoIsExplicitPrivateAndUsesOnlyCurrentSavedCredentials(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := credentials.NewMemoryStore()
	m.SetCredentialStore(vault)
	j, err := m.Submit(jobs.Spec{ID: "account-info", Email: "info@example.test", ImportOnly: true, Input: map[string]any{
		"password": "Old-fixture-password9!", "totpSecret": resetAPISeed, "mailApiUrl": "https://old-mail.example.test",
	}})
	if err != nil {
		t.Fatal(err)
	}
	current := credentials.Credentials{
		Password: "Current-fixture-password9!", TOTPSecret: "GEZDGNBVGY3TQOJQ",
		MailAPIURL: "https://current-mail.example.test", MailMethod: "POST", MailBody: "current-mail-fixture",
		MailHeaders: map[string]string{"Authorization": "current-mail-header-fixture"}, ProxyURL: "http://fixture-user:fixture-pass@proxy.example.test:80",
		PendingPassword: "Unverified-password-fixture", PendingTOTPSecret: "UNVERIFIEDFACTOR", PendingTOTPURI: "unverified-uri-fixture",
	}
	if err := vault.Save(j.Email, current); err != nil {
		t.Fatal(err)
	}
	path := "/api/jobs/" + j.ID + "/account-info"
	for _, token := range []string{"", "invalid"} {
		w := frontendRequest(s, http.MethodGet, path, nil, token)
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), current.Password) {
			t.Fatal("account information is not token protected")
		}
	}
	if w := frontendRequest(s, http.MethodPost, path, nil, "test"); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
		t.Fatal("account information accepted a mutation method")
	}
	w := frontendRequest(s, http.MethodGet, path, nil, "test")
	var got accountInfoResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || got.Password != current.Password || got.TOTPSecret != current.TOTPSecret || got.MailAPIURL != current.MailAPIURL || got.MailRequestBody != current.MailBody || got.MailRequestMethod != "POST" || got.MailRequestHeaders["Authorization"] != current.MailHeaders["Authorization"] || got.ProxyURL != current.ProxyURL || got.CredentialSource != "secure-store" || got.TOTPStatus != "saved" {
		t.Fatal("account information did not use the current saved credential record")
	}
	for _, secret := range []string{"Old-fixture-password9!", resetAPISeed, current.PendingPassword, current.PendingTOTPSecret, current.PendingTOTPURI} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("account information returned stale or unverified material")
		}
	}
	for _, publicPath := range []string{"/api/jobs", "/api/jobs/" + j.ID, "/api/jobs/" + j.ID + "/logs"} {
		public := frontendRequest(s, http.MethodGet, publicPath, nil, "test")
		for _, secret := range []string{current.Password, current.TOTPSecret, current.MailBody, current.ProxyURL, current.PendingTOTPSecret, "Old-fixture-password9!", resetAPISeed} {
			if strings.Contains(public.Body.String(), secret) {
				t.Fatal("private information was included in public polling or logs", publicPath)
			}
		}
	}
	unchanged, err := vault.Load(j.Email)
	if err != nil || unchanged.Password != current.Password || unchanged.PendingTOTPSecret != current.PendingTOTPSecret {
		t.Fatal("information request changed credentials")
	}
	_ = vault.Delete(j.Email)
	w = frontendRequest(s, http.MethodGet, path, nil, "test")
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "Old-fixture-password9!") || strings.Contains(w.Body.String(), resetAPISeed) {
		t.Fatal("missing saved credentials fell back to stale process-local inputs")
	}
}

func TestAccountInfoSupportsBareEmailAndRejectsMissingAccount(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	j, err := m.Submit(jobs.Spec{ID: "bare-info", Email: "bare@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	w := frontendRequest(s, http.MethodGet, "/api/jobs/"+j.ID+"/account-info", nil, "test")
	var got accountInfoResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got.Email != j.Email || got.Password != "" || got.TOTPSecret != "" || got.CredentialSource != "none" || got.TOTPStatus != "none" || got.MailRequestHeaders == nil {
		t.Fatal("bare-email information is unavailable or misleading")
	}
	if w := frontendRequest(s, http.MethodGet, "/api/jobs/missing/account-info", nil, "test"); w.Code != http.StatusNotFound {
		t.Fatal("missing account information did not return not found")
	}
}

func TestTOTPResetExportAndAccountInfoUseCommittedFactorAfterCompletionAndRestore(t *testing.T) {
	const newSeed = "GEZDGNBVGY3TQOJQ"
	for _, scenario := range []string{"successful-reset", "logout-failed-after-reset", "reset-failed-before-disable"} {
		t.Run(scenario, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
			m.SetCredentialStore(vault)
			j := importTOTPActionFixture(t, m, "current-factor", true)
			peer, err := m.Submit(jobs.Spec{ID: "older-row", Email: j.Email, ImportOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			m.SetRunner(func(ctx context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
				if scenario == "reset-failed-before-disable" {
					return errors.New("synthetic preflight failure")
				}
				if err := m.StartTOTPWorkflowStep(ctx, active.ID, "totp"); err != nil {
					return err
				}
				if err := m.BeginTOTPReset(ctx, active.ID); err != nil {
					return err
				}
				c, err := vault.Load(active.Email)
				if err != nil {
					return err
				}
				c.TOTPSecret, c.TOTPResetPending = newSeed, false
				if err := vault.Save(active.Email, c); err != nil {
					return err
				}
				if err := m.CommitTOTPReset(ctx, active.ID, c); err != nil {
					return err
				}
				if scenario == "logout-failed-after-reset" {
					if err := m.StartTOTPWorkflowStep(ctx, active.ID, "logout"); err != nil {
						return err
					}
					failure := errors.New("synthetic logout failure")
					if err := m.UpdateTOTPResetResult(active.ID, map[string]any{
						"logout_all_devices_requested": true, "logout_all_devices_status": "failed",
						"logout_all_devices_error": failure.Error(),
					}, failure); err != nil {
						return err
					}
					_ = m.FinishTOTPWorkflow(active.ID, failure)
					return failure
				}
				return m.FinishTOTPWorkflow(active.ID, nil)
			})
			options := map[string]any{"logoutAllDevicesAfterTotp": scenario == "logout-failed-after-reset"}
			if _, err := m.QueueOperation(j.ID, "reset-2fa", options); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{j.ID, peer.ID} {
				for _, path := range []string{"/api/jobs/" + id + "/account-info", "/api/jobs/export-source"} {
					method, body := http.MethodGet, any(nil)
					if strings.HasSuffix(path, "export-source") {
						method, body = http.MethodPost, map[string]any{"ids": []string{id}}
					}
					if w := frontendRequest(s, method, path, body, "test"); w.Code != http.StatusConflict || strings.Contains(w.Body.String(), resetAPISeed) {
						t.Fatal("queued reset exposed a soon-to-be-invalid factor", path)
					}
				}
			}
			m.Start()
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			finished := waitConsoleState(t, m, j.ID, func(j *jobs.Job) bool { return j.FinishedAt != nil })
			if scenario == "logout-failed-after-reset" && (finished.LogoutAllDevicesStatus != "failed" || finished.TOTPResetPhase != "credential_persisted" || finished.OperationStatus != "failed") {
				t.Fatal("synthetic logout failure did not preserve a committed reset")
			}
			wantSeed := newSeed
			if scenario == "reset-failed-before-disable" {
				wantSeed = resetAPISeed
			}
			checkCurrent := func() {
				t.Helper()
				for _, id := range []string{j.ID, peer.ID} {
					w := frontendRequest(s, http.MethodPost, "/api/jobs/export-source", map[string]any{"ids": []string{id}}, "test")
					want := "\ufeff" + j.Email + "----Reset-api-fixture9!----" + wantSeed + "\n"
					if w.Code != http.StatusOK || w.Body.String() != want {
						t.Fatal("source export did not use the currently valid factor", id, w.Code)
					}
					info := frontendRequest(s, http.MethodGet, "/api/jobs/"+id+"/account-info", nil, "test")
					var got accountInfoResponse
					if json.Unmarshal(info.Body.Bytes(), &got) != nil || info.Code != http.StatusOK || got.TOTPSecret != wantSeed {
						t.Fatal("account card does not agree with source export", id, info.Code)
					}
				}
			}
			checkCurrent()
			if err := m.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			st, err := store.New(s.cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			restarted := jobs.NewManager(context.Background(), 1, st, slog.Default())
			restarted.SetCredentialStore(vault)
			if err := restarted.Restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			s.jobs = restarted
			t.Cleanup(func() { _ = restarted.Shutdown(context.Background()) })
			checkCurrent()
		})
	}
}

func TestAccountInfoAndExportRejectQuarantinedTOTPForEverySameEmailRow(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	j := importTOTPActionFixture(t, m, "quarantined-info", true)
	peer, err := m.Submit(jobs.Spec{ID: "quarantined-peer", Email: j.Email, ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.QueueOperation(j.ID, "reset-2fa", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginTOTPReset(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	m.Cancel(j.ID)
	for _, id := range []string{j.ID, peer.ID} {
		info := frontendRequest(s, http.MethodGet, "/api/jobs/"+id+"/account-info", nil, "test")
		export := frontendRequest(s, http.MethodPost, "/api/jobs/export-source", map[string]any{"ids": []string{id}}, "test")
		for _, w := range []struct {
			code int
			body string
		}{{info.Code, info.Body.String()}, {export.Code, export.Body.String()}} {
			if w.code != http.StatusConflict || strings.Contains(w.body, resetAPISeed) || strings.Contains(w.body, "Reset-api-fixture9!") {
				t.Fatal("quarantined factor was returned through private information or source export", id)
			}
		}
	}
}
