package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/transport"
)

// Simulates the protected store without touching the user's OS keychain.
// A new manager reuses this store to exercise process restart recovery.
type resetRunnerVault struct {
	*credentials.MemoryStore
	failCommit atomic.Bool
}

func (v *resetRunnerVault) Save(email string, c credentials.Credentials) error {
	if !c.TOTPResetPending && c.TOTPSecret == "JBSWY3DPEHPK3PXP" && v.failCommit.Swap(false) {
		return errors.New("fixture protected store temporarily unavailable")
	}
	return v.MemoryStore.Save(email, c)
}

func TestTOTPResetPreflightRequiresDurableStorageAndPreservesEvidence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "job", "totp-setup-result.json")
	if err := validateTOTPResetStart(root, "job", "reset@example.test", false); err == nil {
		t.Fatal("reset allowed without durable vault")
	}
	if err := validateTOTPResetStart(root, "job", "reset@example.test", true); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WritePrivateJSON(path, map[string]any{"reset_totp_requested": true, "reset_totp_attempted_at": "fixture-intent", "secret": "fixture-private-evidence"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := validateTOTPResetStart(root, "job", "reset@example.test", true); err == nil {
		t.Fatal("unresolved reset allowed")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("preflight modified private evidence")
	}
}

func TestTOTPResetProductionRunner(t *testing.T) {
	for _, scenario := range []string{"saved", "saved-and-logout", "activation-unverified", "persistence-recovery", "logout-unconfirmed"} {
		t.Run(scenario, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			const email = "reset-runner@example.test"
			const oldSeed = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
			const newSeed = "JBSWY3DPEHPK3PXP"
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			vault.failCommit.Store(scenario == "persistence-recovery")
			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			cfg.ProxyURL, cfg.ProxyURLs = "", nil
			cfg.SameProxyRetries = 0
			st, err := store.New(cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := jobs.NewManager(t.Context(), 1, st, logger)
			t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
			manager.SetCredentialStore(vault)
			job, err := manager.Submit(jobs.Spec{ID: "reset-runner", Email: email, ImportOnly: true, Input: map[string]any{"password": "fixture-password", "totpSecret": oldSeed}})
			if err != nil {
				t.Fatal(err)
			}
			logout := scenario != "saved"
			job, err = manager.QueueOperation(job.ID, "reset-2fa", map[string]any{"resetTotp": true, "logoutAllDevicesAfterTotp": logout})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var calls []string
			info := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				step := r.URL.Path
				if strings.HasSuffix(step, "/mfa_info") {
					info++
					step = fmt.Sprintf("info%d", info)
				}
				calls = append(calls, step)
				w.Header().Set("Content-Type", "application/json")
				factor := func(id string) {
					factors := []any{}
					if id != "" {
						factors = append(factors, map[string]any{"id": id, "factor_type": "totp", "is_recovery": false})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"mfa_enabled_v2": id != "", "factors": map[string]any{"totp": factors}})
				}
				switch step {
				case "/":
					if r.URL.Query().Get("action") != "enable" {
						t.Error("unexpected login request")
					}
					fmt.Fprint(w, `{"accessToken":"fixture-access-token-at-least-twenty-characters"}`)
				case "/api/auth/session":
					_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "fixture-account"}, "user": map[string]any{"email": email}})
				case "info1":
					factor("old-factor")
				case "info2":
					factor("")
				case "info3":
					if scenario == "activation-unverified" {
						http.Error(w, "fixture verification unavailable", http.StatusServiceUnavailable)
						return
					}
					factor("new-factor")
				case "/backend-api/accounts/mfa/user/disable_in_house":
					c, _ := vault.Load(email)
					j, _ := manager.Get(job.ID)
					if !c.TOTPResetPending || !j.TOTPRecoveryPending {
						t.Error("factor removed before durable quarantine")
					}
					fmt.Fprint(w, `{"message":"Factor disabled"}`)
				case "/backend-api/accounts/mfa/enroll":
					fmt.Fprintf(w, `{"secret":%q,"session_id":"new-session","factor":{"id":"new-factor","factor_type":"totp","is_recovery":false}}`, newSeed)
				case "/backend-api/accounts/mfa/user/activate_enrollment":
					c, _ := vault.Load(email)
					if !c.TOTPResetPending || c.PendingTOTPSecret != newSeed {
						t.Error("activation before protected pending seed")
					}
					fmt.Fprint(w, `{"success":true}`)
				case "/backend-api/accounts/logout_all":
					c, _ := vault.Load(email)
					j, _ := manager.Get(job.ID)
					if c.TOTPResetPending || c.TOTPSecret != newSeed || c.PendingTOTPSecret != "" || j.TOTPRecoveryPending || j.TOTPResetPhase != "credential_persisted" {
						t.Error("logout before persistence verification")
					}
					if scenario == "logout-unconfirmed" {
						http.Error(w, "fixture logout unavailable", http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Error("unexpected request", step)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			cfg.ChatGPTBase, cfg.AuthBase = server.URL, server.URL
			cp := protocol.Checkpoint{Version: 1, Stage: "email_verified", Email: email, ChatGPTBase: server.URL, AuthBase: server.URL, Cookies: []transport.Cookie{{Name: "__Secure-next-auth.session-token", Value: "fixture-session", Domain: "127.0.0.1", Path: "/", HostOnly: true}}, Web: map[string]any{"deviceId": "fixture-device"}}
			protocol.BindTransportSession(&cp, legacyProfilePrefix+"chrome_146", transport.ProfileFor("chrome_146").UserAgent, "", false)
			checkpointPath := filepath.Join(cfg.OutputRoot, job.ID, "login-checkpoint.json")
			journalPath := filepath.Join(cfg.OutputRoot, job.ID, "totp-setup-result.json")
			if err := protocol.WriteCheckpoint(checkpointPath, cp); err != nil {
				t.Fatal(err)
			}
			runner := makeJobRunnerWithCredentials(cfg, manager, logger, vault, true)
			err = runner(t.Context(), job, func(jobs.JobEvent) {})
			recovery := scenario == "activation-unverified" || scenario == "persistence-recovery"
			if (err != nil) != (recovery || scenario == "logout-unconfirmed") {
				t.Fatalf("unexpected result: %v", err)
			}
			want := []string{"/", "/api/auth/session", "info1", "/backend-api/accounts/mfa/user/disable_in_house", "info2", "/backend-api/accounts/mfa/enroll", "/backend-api/accounts/mfa/user/activate_enrollment", "info3"}
			if logout && !recovery {
				want = append(want, "/backend-api/accounts/logout_all")
			}
			mu.Lock()
			got := append([]string(nil), calls...)
			mu.Unlock()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("request sequence: %v", got)
			}
			if _, err := os.Stat(checkpointPath); !os.IsNotExist(err) {
				t.Fatal("stale login checkpoint retained")
			}
			c, _ := vault.Load(email)
			j, _ := manager.Get(job.ID)
			meta, _ := os.ReadFile(filepath.Join(cfg.OutputRoot, "job-meta.json"))
			for _, secret := range []string{oldSeed, newSeed, "fixture-password", "otpauth://"} {
				if strings.Contains(string(meta), secret) {
					t.Fatal("plaintext secret in metadata")
				}
			}
			if recovery {
				if !c.TOTPResetPending || c.PendingTOTPSecret != newSeed || !j.TOTPRecoveryPending {
					t.Fatal("unverified activated seed not preserved and blocked")
				}
				b, readErr := os.ReadFile(journalPath)
				var result map[string]any
				if readErr != nil || json.Unmarshal(b, &result) != nil || result["activation_succeeded"] != true || result["activation_verified"] != (scenario == "persistence-recovery") {
					t.Fatal("activation evidence lost")
				}
				if err := runner(t.Context(), job, func(jobs.JobEvent) {}); err == nil {
					t.Fatal("uncertain reset replayed")
				}
				restarted := jobs.NewManager(t.Context(), 1, st, logger)
				restarted.SetCredentialStore(vault)
				if err := restarted.Restore(t.Context()); err != nil {
					t.Fatal(err)
				}
				rj, _ := restarted.Get(job.ID)
				if scenario == "persistence-recovery" {
					saved, _ := vault.Load(email)
					if rj == nil || rj.TOTPRecoveryPending || !rj.HasTOTPKey || saved.TOTPSecret != newSeed || saved.TOTPResetPending || rj.LogoutAllDevicesStatus != "skipped" {
						t.Fatal("locally verified recovery did not save new seed without logout")
					}
					if _, e := os.Stat(journalPath); !os.IsNotExist(e) {
						t.Fatal("durably recovered journal not cleaned")
					}
				} else {
					if rj == nil || !rj.TOTPRecoveryPending || rj.Status != jobs.StatusFailed {
						t.Fatal("restart failed to retain quarantine")
					}
					if _, err := restarted.Retry(job.ID); err == nil {
						t.Fatal("restart permits reset replay")
					}
				}
				_ = restarted.Shutdown(context.Background())
				mu.Lock()
				count := len(calls)
				mu.Unlock()
				if count != len(want) {
					t.Fatal("recovery performed remote mutation")
				}
			} else {
				if c.TOTPSecret != newSeed || c.TOTPResetPending || c.PendingTOTPSecret != "" || j.TOTPRecoveryPending || !j.HasTOTPKey {
					t.Fatal("verified new seed not committed")
				}
				if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
					t.Fatal("committed journal not cleaned")
				}
				wantLogout := "not_requested"
				if logout {
					wantLogout = "succeeded"
				}
				if scenario == "logout-unconfirmed" {
					wantLogout = "unknown"
				}
				if j.LogoutAllDevicesStatus != wantLogout {
					t.Fatalf("logout outcome: %s", j.LogoutAllDevicesStatus)
				}
			}
		})
	}
}
