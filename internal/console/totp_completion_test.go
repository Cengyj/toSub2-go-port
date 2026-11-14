package console

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

func completionPublicJob(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	response := frontendRequest(s, http.MethodGet, "/api/jobs/"+id, nil, "test")
	var out map[string]any
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &out) != nil {
		t.Fatalf("public job unavailable: %d", response.Code)
	}
	return out
}

func completionAPIFixture(t *testing.T, source string) (*Server, *jobs.Manager, *jobs.Job) {
	t.Helper()
	s, m, j, vault := recoveryAPIWithVault(t)
	result := completionFixtureResult(j.Email, "fixture-operation", completedResetSeed, source, false)
	if err := protocol.WritePrivateJSON(filepath.Join(s.cfg.OutputRoot, j.ID, "totp-setup-result.json"), result); err != nil {
		t.Fatal(err)
	}
	c, err := vault.Load(j.Email)
	if err != nil {
		t.Fatal(err)
	}
	c.PendingTOTPSecret = completedResetSeed
	if err := vault.Save(j.Email, c); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTOTPResetResult(j.ID, result, nil); err != nil {
		t.Fatal(err)
	}
	return s, m, j
}

func TestTOTPCompletionAPIUsesOnlyBoundActivationEvidence(t *testing.T) {
	for _, source := range []string{"business_receipt", "state_read"} {
		t.Run(source, func(t *testing.T) {
			s, m, j := completionAPIFixture(t, source)
			public := completionPublicJob(t, s, j.ID)
			if public["canCompleteTotp"] != true || public["totpCompletionPending"] != true || public["canResetTotp"] != false || public["canDownload"] != false {
				t.Fatal("confirmed activation did not expose isolated local completion", public)
			}
			for _, key := range []string{"canRecoverTotp", "canInspectTotpRecovery", "canContinueTotp", "totpReauthenticationRequired", "totpReauthenticationActive", "totpReauthenticationStep", "totpPreparedPending", "totpInspectionState", "totpInspectionMessage", "totpRecoveryHint", "totpSessionDirty", "totpHandoffPending", "totpCookiesCleaned"} {
				if _, exists := public[key]; exists {
					t.Fatalf("removed projection remains: %s", key)
				}
			}
			if !strings.Contains(public["totpCompletionHint"].(string), "不连接远端") {
				t.Fatal("local completion boundary missing")
			}
			response := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/complete-2fa", map[string]any{}, "test")
			queued, _ := m.Get(j.ID)
			if response.Code != http.StatusOK || queued.Operation != "complete-2fa" || m.QueuePosition(j.ID) != 1 || queued.LogoutAllDevicesAfterTOTP || queued.ReloginAfterTOTP || queued.UploadAfterTOTP {
				t.Fatalf("local completion incorrectly queued: %d", response.Code)
			}
			if again := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/complete-2fa", map[string]any{}, "test"); again.Code != http.StatusConflict || m.QueuePosition(j.ID) != 1 {
				t.Fatal("duplicate completion admitted")
			}
		})
	}
}

func TestTOTPLegacyRoutesNeverFallbackToResetOrCompletion(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "confirmed"}[ready], func(t *testing.T) {
			var s *Server
			var m *jobs.Manager
			var j *jobs.Job
			if ready {
				s, m, j = completionAPIFixture(t, "business_receipt")
			} else {
				s, m, j = recoveryAPIFixture(t)
			}
			path := filepath.Join(s.cfg.OutputRoot, j.ID, "totp-setup-result.json")
			before, _ := os.ReadFile(path)
			for _, route := range []string{"/api/jobs/" + j.ID + "/recover-2fa", "/api/jobs/" + j.ID + "/inspect-2fa", "/api/jobs/recover-2fa-batch", "/api/jobs/inspect-2fa-batch", "/api/jobs/complete-2fa-batch"} {
				r := frontendRequest(s, http.MethodPost, route, map[string]any{"ids": []string{j.ID}}, "test")
				if r.Code != http.StatusNotFound {
					t.Fatalf("removed route admitted: %s %d", route, r.Code)
				}
			}
			reset := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/setup-2fa", map[string]any{"resetTotp": true}, "test")
			if reset.Code != http.StatusConflict {
				t.Fatal("ordinary reset became a completion or remote recovery alias", reset.Code)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) || m.QueuePosition(j.ID) != 0 {
				t.Fatal("rejected route changed operation evidence")
			}
			if !ready {
				public := completionPublicJob(t, s, j.ID)
				if public["canCompleteTotp"] != false || public["canResetTotp"] != false || public["canSetupTotp"] != false || public["canForceRelogin"] != false {
					t.Fatal("unknown state acquired action", public)
				}
				if r := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/complete-2fa", map[string]any{}, "test"); r.Code != http.StatusConflict {
					t.Fatal("unknown state admitted completion", r.Code)
				}
			}
		})
	}
}

func TestTOTPCompletionAPIRejectsInputsAndContradictoryEvidence(t *testing.T) {
	s, m, j := completionAPIFixture(t, "business_receipt")
	for _, key := range []string{"password", "totpSecret", "totpCode", "proxyUrl", "resetTotp", "logoutAllDevicesAfterTotp", "reloginAfterTotp", "uploadAfterTotp", "sub2apiConfig", "reauthenticate"} {
		r := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/complete-2fa", map[string]any{key: true}, "test")
		if r.Code != http.StatusBadRequest || m.QueuePosition(j.ID) != 0 {
			t.Fatalf("completion accepted %s: %d", key, r.Code)
		}
	}
	for _, value := range []any{nil, true, 123, []any{"intent"}} {
		r := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/complete-2fa", map[string]any{"totpIntentId": value}, "test")
		if r.Code != http.StatusBadRequest {
			t.Fatalf("completion accepted invalid intent: %d", r.Code)
		}
	}
	for _, action := range []string{"setup-2fa", "relogin", "regenerate", "add-password"} {
		if r := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/"+action, map[string]any{"reauthenticate": true}, "test"); r.Code != http.StatusBadRequest {
			t.Fatal("removed authentication option was accepted", action, r.Code)
		}
	}
	for _, scenario := range []string{"missing-secret", "wrong-operation", "contradictory-source", "wrong-account"} {
		t.Run(scenario, func(t *testing.T) {
			s, m, j := completionAPIFixture(t, "business_receipt")
			path := filepath.Join(s.cfg.OutputRoot, j.ID, "totp-setup-result.json")
			result, err := protocol.ReadTOTPResetJournal(path)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing-secret":
				delete(result, "secret")
			case "wrong-operation":
				result["operation_id"] = "another-operation"
			case "contradictory-source":
				result["activation_confirmed_by_read"] = true
			case "wrong-account":
				result["email"] = "other@synthetic.invalid"
			}
			if err := protocol.WritePrivateJSON(path, result); err != nil {
				t.Fatal(err)
			}
			if public := completionPublicJob(t, s, j.ID); public["canCompleteTotp"] != false {
				t.Fatal("invalid evidence offered completion", scenario)
			}
			r := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/complete-2fa", map[string]any{}, "test")
			if r.Code != http.StatusConflict || m.QueuePosition(j.ID) != 0 {
				t.Fatal("invalid evidence admitted", scenario, r.Code)
			}
		})
	}
}

func TestTOTPCompletionIntentReplayPrecedesCapabilityGate(t *testing.T) {
	s, m, j := completionAPIFixture(t, "business_receipt")
	body := map[string]any{"totpIntentId": "synthetic-local-completion-intent"}
	for i := 0; i < 2; i++ {
		response := frontendRequest(s, http.MethodPost, "/api/jobs/"+j.ID+"/complete-2fa", body, "test")
		if response.Code != http.StatusOK || m.QueuePosition(j.ID) != 1 {
			t.Fatalf("completion intent was not replayed without duplicate queue: %d", response.Code)
		}
	}
	current, _ := m.Get(j.ID)
	if current.Operation != "complete-2fa" {
		t.Fatal("replay changed local operation")
	}
}
