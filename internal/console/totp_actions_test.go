package console

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

const resetAPISeed = "JBSWY3DPEHPK3PXP"

func importTOTPActionFixture(t *testing.T, m *jobs.Manager, id string, enabled bool) *jobs.Job {
	t.Helper()
	input := map[string]any{"password": "Reset-api-fixture9!"}
	if enabled {
		input["totpSecret"] = resetAPISeed
	}
	j, err := m.Submit(jobs.Spec{ID: id, Email: id + "@example.test", ImportOnly: true, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestTOTPActionAPIRejectsNonBooleanOptionsBeforeMutation(t *testing.T) {
	invalid := []struct {
		name  string
		value any
	}{
		{"null", nil}, {"string-true", "true"}, {"string-false", "false"},
		{"one", 1}, {"zero", 0}, {"object", map[string]any{}}, {"array", []any{true}},
	}
	for _, batch := range []bool{false, true} {
		mode := "single"
		if batch {
			mode = "batch"
		}
		for _, field := range []string{"resetTotp", "logoutAllDevicesAfterTotp", "reloginAfterTotp", "uploadAfterTotp"} {
			for _, value := range invalid {
				t.Run(mode+"/"+field+"/"+value.name, func(t *testing.T) {
					s, m := newMonitorTestServer(t)
					m.SetCredentialStore(credentials.NewMemoryStore())
					j := importTOTPActionFixture(t, m, "strict-options", true)
					body := map[string]any{"resetTotp": true, field: value.value}
					path := "/api/jobs/" + j.ID + "/setup-2fa"
					if batch {
						path, body["ids"] = "/api/jobs/setup-2fa-batch", []string{j.ID}
					}
					w := frontendRequest(s, http.MethodPost, path, body, "test")
					if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), field) {
						t.Fatalf("malformed boolean was not rejected: status=%d body=%s", w.Code, w.Body.String())
					}
					got, _ := m.Get(j.ID)
					if got.Status != jobs.StatusImported || got.Operation != j.Operation || got.ResetTOTP || got.LogoutAllDevicesAfterTOTP || m.QueuePosition(j.ID) != 0 {
						t.Fatal("invalid request changed or queued the account")
					}
				})
			}
		}
	}
}

func TestTOTPWorkflowAPIValidatesDependenciesBeforeQueueing(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, scenario := range []string{"upload-without-relogin", "missing-upload-config", "bad-upload-config", "valid-followups"} {
			t.Run(map[bool]string{false: "single/", true: "batch/"}[batch]+scenario, func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				m.SetCredentialStore(credentials.NewMemoryStore())
				j := importTOTPActionFixture(t, m, "workflow-options", true)
				body := map[string]any{"resetTotp": true, "logoutAllDevicesAfterTotp": true, "reloginAfterTotp": true, "uploadAfterTotp": true}
				cfg := map[string]any{"baseUrl": "https://workflow-sub2.example.test", "adminApiKey": "workflow-API-key-canary", "groupIds": []string{"11"}, "priority": "0"}
				body["sub2apiConfig"] = cfg
				switch scenario {
				case "upload-without-relogin":
					body["reloginAfterTotp"] = false
				case "missing-upload-config":
					delete(body, "sub2apiConfig")
				case "bad-upload-config":
					cfg["baseUrl"] = "ftp://invalid.example.test"
				}
				path := "/api/jobs/" + j.ID + "/setup-2fa"
				if batch {
					path, body["ids"] = "/api/jobs/setup-2fa-batch", []string{j.ID}
				}
				w := frontendRequest(s, http.MethodPost, path, body, "test")
				got, _ := m.Get(j.ID)
				if scenario != "valid-followups" {
					if w.Code != http.StatusBadRequest || got.Status != jobs.StatusImported || got.ResetTOTP || got.ReloginAfterTOTP || got.UploadAfterTOTP || m.QueuePosition(j.ID) != 0 {
						t.Fatalf("invalid configuration mutated account: %d %s", w.Code, w.Body.String())
					}
				} else {
					if w.Code != http.StatusOK || got.Status != jobs.StatusQueued || !got.ResetTOTP || !got.ReloginAfterTOTP || !got.UploadAfterTOTP || got.TOTPWorkflowStatus != "pending" {
						t.Fatalf("valid workflow not queued: %d %s", w.Code, w.Body.String())
					}
					metadata, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
					if err != nil {
						t.Fatal(err)
					}
					for _, required := range []string{"\"relogin_after_totp\": true", "\"upload_after_totp\": true", "\"totp_workflow_status\": \"pending\""} {
						if !strings.Contains(string(metadata), required) {
							t.Fatal("workflow selection not persisted", required)
						}
					}
					if strings.Contains(string(metadata), "workflow-API-key-canary") || strings.Contains(string(metadata), "workflow-sub2.example.test") {
						t.Fatal("private upload config persisted")
					}
				}
				if strings.Contains(w.Body.String(), "workflow-API-key-canary") || strings.Contains(w.Body.String(), resetAPISeed) {
					t.Fatal("workflow response exposed credentials")
				}
			})
		}
	}
}

func TestTOTPActionAPIRejectsLogoutWithoutExplicitReset(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, action := range []string{"setup-2fa", "add-password", "relogin"} {
			t.Run(action+map[bool]string{false: "/single", true: "/batch"}[batch], func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				m.SetCredentialStore(credentials.NewMemoryStore())
				j := importTOTPActionFixture(t, m, "logout-option", false)
				body := map[string]any{"logoutAllDevicesAfterTotp": true}
				path := "/api/jobs/" + j.ID + "/" + action
				if batch {
					path, body["ids"] = "/api/jobs/"+action+"-batch", []string{j.ID}
				}
				w := frontendRequest(s, http.MethodPost, path, body, "test")
				if w.Code != http.StatusBadRequest {
					t.Fatalf("logout outside reset accepted: %d %s", w.Code, w.Body.String())
				}
				got, _ := m.Get(j.ID)
				if got.Status != jobs.StatusImported || m.QueuePosition(j.ID) != 0 {
					t.Fatal("invalid logout request queued account")
				}
			})
		}
	}
}

func TestTOTPActionAPIAdvertisesSeparateSetupAndResetEligibility(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	bootstrap := frontendRequest(s, http.MethodGet, "/api/bootstrap", nil, "test")
	var capabilities struct {
		Features map[string]json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	for _, feature := range []string{"totpSetup", "totpReset", "logoutAllDevicesAfterTotp"} {
		if string(capabilities.Features[feature]) != "true" {
			t.Fatal("missing feature advertisement", feature)
		}
	}
	for _, tc := range []struct {
		id      string
		seed    bool
		known   bool
		queued  bool
		setupOK bool
		resetOK bool
	}{
		{"new-account", false, false, false, true, false},
		{"with-seed", true, false, false, false, true},
		{"known-enabled", false, true, false, false, true},
		{"busy-account", true, false, true, false, false},
	} {
		j := importTOTPActionFixture(t, m, tc.id, tc.seed)
		if tc.known {
			m.UpdateTOTPSetup(j.ID, "", "", "", true)
		}
		if tc.queued {
			if _, err := m.QueueOperation(j.ID, "relogin", nil); err != nil {
				t.Fatal(err)
			}
		}
		w := frontendRequest(s, http.MethodGet, "/api/jobs/"+j.ID, nil, "test")
		var public map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil || w.Code != http.StatusOK {
			t.Fatalf("read job: %v status=%d", err, w.Code)
		}
		if public["canSetupTotp"] != tc.setupOK || public["canResetTotp"] != tc.resetOK {
			t.Fatalf("wrong setup/reset eligibility for %s", tc.id)
		}
		if !tc.setupOK && public["totpSetupUnavailableReason"] == "" || !tc.resetOK && public["totpResetUnavailableReason"] == "" {
			t.Fatalf("missing disabled reason for %s", tc.id)
		}
		for _, reset := range []bool{false, true} {
			if reset && tc.resetOK || !reset && tc.setupOK {
				continue
			}
			w = frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/setup-2fa", map[string]any{"resetTotp": reset}, "test")
			if w.Code != http.StatusConflict {
				t.Fatalf("ineligible operation accepted for %s reset=%v: %d", tc.id, reset, w.Code)
			}
		}
	}
	w := frontendRequest(s, http.MethodGet, "/api/jobs?pageSize=1", nil, "test")
	var page struct {
		Jobs      []map[string]any `json:"jobs"`
		Selection []map[string]any `json:"selection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Jobs) != 1 || len(page.Selection) != 4 {
		t.Fatalf("invalid paginated eligibility response: %v", err)
	}
	for _, row := range page.Selection {
		if _, ok := row["canResetTotp"].(bool); !ok {
			t.Fatal("cross-page selection lacks reset eligibility")
		}
		if strings.Contains(string(w.Body.Bytes()), resetAPISeed) {
			t.Fatal("eligibility response leaked a TOTP seed")
		}
	}
}

func TestTOTPActionAPIQueuesAndPersistsExplicitOperation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reset  bool
		logout bool
		omit   bool
	}{
		{"setup-default", false, false, true}, {"setup-explicit-false", false, false, false},
		{"reset-default-logout", true, false, false}, {"reset-with-logout", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			m.SetCredentialStore(credentials.NewMemoryStore())
			j := importTOTPActionFixture(t, m, "operation", tc.reset)
			body := map[string]any{}
			if !tc.omit {
				body["resetTotp"], body["logoutAllDevicesAfterTotp"] = tc.reset, tc.logout
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/setup-2fa", body, "test")
			if w.Code != http.StatusOK {
				t.Fatalf("operation rejected: %d %s", w.Code, w.Body.String())
			}
			got, _ := m.Get(j.ID)
			operation, logoutStatus := "setup-2fa", "not_requested"
			if tc.reset {
				operation = "reset-2fa"
			}
			if tc.logout {
				logoutStatus = "pending"
			}
			if got.Operation != operation || got.ResetTOTP != tc.reset || got.LogoutAllDevicesAfterTOTP != tc.logout || got.LogoutAllDevicesStatus != logoutStatus || got.Status != jobs.StatusQueued || got.SecurityActionReturnStatus != jobs.StatusImported {
				t.Fatal("explicit setup/reset intent did not reach the queued operation")
			}
			metadata, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, "job-meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Jobs []map[string]any `json:"jobs"`
			}
			if err := json.Unmarshal(metadata, &saved); err != nil || len(saved.Jobs) != 1 {
				t.Fatal("invalid persisted metadata", err)
			}
			state := saved.Jobs[0]
			if state["operation"] != operation || state["reset_totp"] != tc.reset || state["logout_all_devices_after_totp"] != tc.logout || state["logout_all_devices_status"] != logoutStatus {
				t.Fatal("queued security options were not persisted")
			}
			for _, secret := range []string{resetAPISeed, "Reset-api-fixture9!"} {
				if strings.Contains(w.Body.String(), secret) || strings.Contains(string(metadata), secret) {
					t.Fatal("operation response or metadata leaked credentials")
				}
			}
		})
	}
}

func TestTOTPActionAPIBatchSkipsIneligibleAccounts(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "setup", true: "reset"}[reset], func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			m.SetCredentialStore(credentials.NewMemoryStore())
			fresh := importTOTPActionFixture(t, m, "fresh", false)
			enabled := importTOTPActionFixture(t, m, "enabled", true)
			busy := importTOTPActionFixture(t, m, "busy", true)
			if _, err := m.QueueOperation(busy.ID, "relogin", nil); err != nil {
				t.Fatal(err)
			}
			w := frontendRequest(s, http.MethodPost, "/api/jobs/setup-2fa-batch", map[string]any{
				"ids": []string{fresh.ID, enabled.ID, busy.ID, enabled.ID}, "resetTotp": reset, "logoutAllDevicesAfterTotp": reset,
			}, "test")
			var result struct {
				Started int              `json:"started"`
				Skipped int              `json:"skipped"`
				Jobs    []map[string]any `json:"jobs"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Started != 1 || result.Skipped != 2 || len(result.Jobs) != 1 {
				t.Fatalf("batch did not select exactly one eligible account: %d %s", w.Code, w.Body.String())
			}
			selected, skipped := fresh.ID, enabled.ID
			if reset {
				selected, skipped = enabled.ID, fresh.ID
			}
			if result.Jobs[0]["id"] != selected || result.Jobs[0]["resetTotp"] != reset || result.Jobs[0]["logoutAllDevicesAfterTotp"] != reset {
				t.Fatal("batch did not preserve selected reset/logout intent")
			}
			unmodified, _ := m.Get(skipped)
			stillBusy, _ := m.Get(busy.ID)
			if unmodified.Status != jobs.StatusImported || stillBusy.Operation != "relogin" || stillBusy.ResetTOTP || stillBusy.LogoutAllDevicesAfterTOTP {
				t.Fatal("batch changed an ineligible account")
			}
		})
	}
}

func TestTOTPActionAPIRecoveryBlocksActionsAndSecretExport(t *testing.T) {
	s, m := newMonitorTestServer(t)
	vault := &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	m.SetCredentialStore(vault)
	j := importTOTPActionFixture(t, m, "recovering", true)
	peer, err := m.Submit(jobs.Spec{ID: "same-account-history", Email: j.Email, ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	w := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/setup-2fa", map[string]any{"resetTotp": true, "logoutAllDevicesAfterTotp": true}, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := m.BeginTOTPReset(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	const newSeed = "GEZDGNBVGY3TQOJQ"
	const uri = "otpauth://totp/fixture?secret=" + newSeed
	if err := m.UpdateTOTPResetResult(j.ID, map[string]any{
		"reset_totp_requested": true, "old_factor_invalidated": true,
		"reset_totp_status": "unknown", "reset_totp_phase": "recovery_required",
		"secret": newSeed, "otpauth_uri": uri, "reset_totp_error": "could not save " + newSeed,
		"logout_all_devices_status": "not_attempted", "logout_all_devices_error": "not submitted " + newSeed,
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, peer.ID} {
		w := frontendRequest(s, http.MethodGet, "/api/jobs/"+id, nil, "test")
		var public map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil || w.Code != http.StatusOK {
			t.Fatal("cannot read public recovery state", err)
		}
		for _, key := range []string{"canSetupTotp", "canResetTotp", "canRetry", "canResume", "canRegenerate", "canForceRelogin", "canAddPassword", "autoRepairEligible"} {
			if public[key] != false {
				t.Fatal("recovery account exposes unsafe action", id, key)
			}
		}
		if public["totpRecoveryPending"] != true || public["totpCredentialInvalidated"] != true || public["totpResetUnavailableReason"] == "" {
			t.Fatal("recovery reason or credential invalidation is missing", id)
		}
		for _, action := range []string{"setup-2fa", "retry", "relogin", "add-password"} {
			body := map[string]any{}
			if action == "setup-2fa" {
				body["resetTotp"] = true
			}
			blocked := frontendRequest(s, http.MethodPost, "/api/jobs/"+id+"/"+action, body, "test")
			if blocked.Code != http.StatusConflict {
				t.Fatal("recovery account accepted action", id, action, blocked.Code)
			}
		}
		if secret := frontendRequest(s, http.MethodGet, "/api/jobs/"+id+"/totp-setup", nil, "test"); secret.Code != http.StatusNotFound {
			t.Fatal("recovery secret remains available through setup route")
		}
		for _, secrets := range []string{resetAPISeed, newSeed, uri, "Reset-api-fixture9!"} {
			if strings.Contains(w.Body.String(), secrets) {
				t.Fatal("public recovery state exposed a credential")
			}
		}
	}
	fresh := importTOTPActionFixture(t, m, "safe-export-first", false)
	for _, ids := range [][]string{{j.ID}, {peer.ID}, {fresh.ID, j.ID}} {
		w := frontendRequest(s, http.MethodPost, "/api/jobs/export-source", map[string]any{"ids": ids}, "test")
		if w.Code != http.StatusConflict || w.Header().Get("Content-Disposition") != "" {
			t.Fatal("recovery export was not rejected atomically", w.Code)
		}
		for _, secret := range []string{resetAPISeed, newSeed, "Reset-api-fixture9!"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("rejected export disclosed current or stale credentials")
			}
		}
	}
}
