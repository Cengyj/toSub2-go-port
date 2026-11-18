package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestRunnerUsesAndLearnsVerifiedFactorBinding(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, existingBinding := range []bool{false, true} {
		name := "legacy-single-factor"
		if existingBinding {
			name = "bound-second-factor"
		}
		for _, visible := range []string{"chosen-factor", "old-factor"} {
			t.Run(name+"/"+visible, func(t *testing.T) {
				remote := &sessionAuditRemote{t: t, sessionValid: true, factor: visible}
				vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
				var verified, bindingObserved atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					write := func(value any) {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(value)
					}
					switch r.URL.Path {
					case "/api/accounts/password/verify":
						factors := []any{map[string]any{"factor_type": "totp", "id": "chosen-factor"}}
						defaultID := "chosen-factor"
						if existingBinding {
							defaultID = "other-factor"
							factors = append([]any{map[string]any{"factor_type": "totp", "id": defaultID}}, factors...)
						}
						write(map[string]any{"continue_url": "http://" + r.Host + "/mfa-challenge/" + defaultID, "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_challenge_factors": factors}})
						return
					case "/api/accounts/mfa/issue_challenge", "/api/accounts/mfa/verify":
						var body map[string]any
						if json.NewDecoder(r.Body).Decode(&body) != nil || body["id"] != "chosen-factor" {
							t.Error("runner sent the saved seed to a different factor")
							w.WriteHeader(http.StatusForbidden)
							return
						}
						if r.URL.Path == "/api/accounts/mfa/verify" {
							verified.Store(true)
							write(map[string]any{"continue_url": "http://" + r.Host + "/api/auth/callback/openai?code=fixture-once"})
						} else {
							write(map[string]any{"success": true})
						}
						return
					case "/backend-api/accounts/mfa_info":
						if remote.snapshot().infoReads == 0 {
							current, err := vault.Load(sessionAuditEmail)
							bindingObserved.Store(err == nil && current.TOTPSecret == sessionAuditOld && current.TOTPFactorID == "chosen-factor")
						}
					}
					remote.serveHTTP(w, r)
				}))
				defer server.Close()
				cfg := sessionAuditConfig(t.TempDir(), server.URL)
				m, _, logger := sessionAuditManager(t, cfg, vault)
				sessionAuditPrepare(t, cfg, m)
				if existingBinding {
					current, err := vault.Load(sessionAuditEmail)
					if err != nil {
						t.Fatal(err)
					}
					current.TOTPFactorID = "chosen-factor"
					if err := vault.Save(sessionAuditEmail, current); err != nil {
						t.Fatal(err)
					}
				}
				err := sessionAuditExecute(t, cfg, m, logger, vault)
				if visible == "chosen-factor" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					s := remote.snapshot()
					if err == nil || s.disableCalls+s.enrollCalls+s.activateCalls != 0 {
						t.Fatal("a different visible factor permitted a reset")
					}
				}
				if !verified.Load() || !bindingObserved.Load() {
					t.Fatal("verified login did not persist the exact factor binding before continuing")
				}
			})
		}
	}
}

func TestCredentialTOTPFactorIDRequiresExactSeedMatch(t *testing.T) {
	saved := credentials.Credentials{TOTPSecret: "active-seed", TOTPFactorID: "active-factor", PendingTOTPSecret: "pending-seed", PendingTOTPFactorID: "pending-factor"}
	for _, tc := range []struct{ candidate, factor string }{{"active-seed", "active-factor"}, {"pending-seed", "pending-factor"}, {"another-seed", ""}, {"", ""}} {
		if got := credentialTOTPFactorID(saved, tc.candidate); got != tc.factor {
			t.Fatal("candidate inherited an unrelated factor binding")
		}
	}
}

func TestOperationCredentialsKeepBindingsWithTheirSeeds(t *testing.T) {
	for _, tc := range []struct {
		name          string
		value         credentials.Credentials
		clear         bool
		id, pendingID string
	}{
		{"unrelated-password-update", credentials.Credentials{Password: "new-password", TOTPSecret: "old-seed"}, false, "old-factor", "pending-factor"},
		{"new-unbound-seed", credentials.Credentials{TOTPSecret: "replacement-seed"}, false, "", "pending-factor"},
		{"new-bound-seed", credentials.Credentials{TOTPSecret: "replacement-seed", TOTPFactorID: "replacement-factor"}, false, "replacement-factor", "pending-factor"},
		{"clear-pending", credentials.Credentials{TOTPSecret: "old-seed"}, true, "old-factor", ""},
		{"new-pending-seed", credentials.Credentials{TOTPSecret: "old-seed", PendingTOTPSecret: "different-pending"}, true, "old-factor", ""},
		{"orphan-ids", credentials.Credentials{TOTPFactorID: "orphan", PendingTOTPFactorID: "orphan-pending"}, true, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vault := credentials.NewMemoryStore()
			if err := vault.Save("owner@example.test", credentials.Credentials{TOTPSecret: "old-seed", TOTPFactorID: "old-factor", PendingTOTPSecret: "pending-seed", PendingTOTPURI: "pending-uri", PendingTOTPFactorID: "pending-factor"}); err != nil {
				t.Fatal(err)
			}
			protect := operationCredentialProtector(t.Context(), vault, "owner@example.test", true, func(credentials.Credentials) {})
			if err := protect(tc.value, tc.clear, false); err != nil {
				t.Fatal(err)
			}
			got, err := vault.Load("owner@example.test")
			if err != nil || got.TOTPFactorID != tc.id || got.PendingTOTPFactorID != tc.pendingID {
				t.Fatal("credential update lost or misassigned a factor binding", err)
			}
		})
	}
}

func TestResetPersistsFactorBeforeActivationAndCommitsMatchingBinding(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	remote := &sessionAuditRemote{t: t, sessionValid: true, factor: "old-factor"}
	vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
	var checkedPending atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/accounts/mfa/user/activate_enrollment" {
			pending, err := vault.Load(sessionAuditEmail)
			if err != nil || pending.PendingTOTPSecret != sessionAuditNew || pending.PendingTOTPFactorID != "new-factor" || !pending.TOTPResetPending {
				t.Error("activation crossed the boundary before seed and factor were durably paired")
			}
			checkedPending.Store(true)
		}
		remote.serveHTTP(w, r)
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	m, _, logger := sessionAuditManager(t, cfg, vault)
	sessionAuditPrepare(t, cfg, m)
	if err := sessionAuditExecute(t, cfg, m, logger, vault); err != nil {
		t.Fatal(err)
	}
	saved, err := vault.Load(sessionAuditEmail)
	if err != nil || !checkedPending.Load() || saved.TOTPFactorID != "new-factor" || saved.TOTPSecret != sessionAuditNew || saved.PendingTOTPFactorID != "" || saved.PendingTOTPSecret != "" {
		t.Fatal("completed reset did not retain the matching active factor binding", err)
	}
}

func TestLocalCompletionBindsLegacyVaultAndRejectsDifferentFactor(t *testing.T) {
	for _, state := range []string{"pending-legacy", "committed-legacy", "pending-conflict", "committed-conflict"} {
		t.Run(state, func(t *testing.T) {
			cfg := sessionAuditConfig(t.TempDir(), "http://127.0.0.1:1")
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			m, _, _ := sessionAuditManager(t, cfg, vault)
			job, err := m.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"totpSecret": sessionAuditOld}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.QueueOperation(job.ID, "reset-2fa", nil); err != nil {
				t.Fatal(err)
			}
			runnerResetIntentJournal(t, cfg.OutputRoot, job.ID, job.Email, "factor-completion")
			if err := m.BeginTOTPReset(t.Context(), job.ID, "factor-completion"); err != nil {
				t.Fatal(err)
			}
			path := runnerCompletionJournal(t, cfg.OutputRoot, job.ID, job.Email, "factor-completion", sessionAuditNew, "not_requested")
			current, err := vault.Load(job.Email)
			if err != nil {
				t.Fatal(err)
			}
			current.PendingTOTPSecret = sessionAuditNew
			if state == "committed-legacy" || state == "committed-conflict" {
				current.TOTPResetPending = false
				current.TOTPSecret, current.PendingTOTPSecret = sessionAuditNew, ""
			}
			if state == "pending-conflict" {
				current.PendingTOTPFactorID = "other-factor"
			}
			if state == "committed-conflict" {
				current.TOTPFactorID = "other-factor"
			}
			if err := vault.Save(job.Email, current); err != nil {
				t.Fatal(err)
			}
			job, _ = m.Get(job.ID)
			err = finalizeTOTPResetLocally(t.Context(), path, m, vault, job)
			after, loadErr := vault.Load(job.Email)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if state == "pending-conflict" || state == "committed-conflict" {
				if err == nil || !reflect.DeepEqual(current, after) {
					t.Fatal("conflicting factor was overwritten", err)
				}
			} else if err != nil || after.TOTPFactorID != "fixture-new-factor" || after.PendingTOTPFactorID != "" || after.TOTPResetPending {
				t.Fatal("legacy material did not acquire its operation-bound factor", err)
			}
		})
	}
}
