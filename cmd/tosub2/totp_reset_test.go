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
	const logoutCanary = "fixture-private-logout-response"
	for _, test := range []struct {
		name, logoutBody, logoutReason string
	}{
		{name: "saved"}, {name: "saved-and-logout"}, {name: "compact-disabled"},
		{name: "null-totp-disabled"}, {name: "activation-unverified"}, {name: "activation-ack-read-unavailable"},
		{name: "persistence-recovery"}, {name: "logout-unconfirmed"},
		{name: "logout-empty-body"},
		{"logout-empty-object", `{}`, ""},
		{"logout-private-fields", `{"vendor_detail":{"opaque":"` + logoutCanary + `"}}`, ""},
		{"logout-null", `null`, ""},
		{"logout-true", `true`, ""},
		{"logout-false", `false`, ""},
		{"logout-array", `["` + logoutCanary + `"]`, ""},
		{"logout-string", `"` + logoutCanary + `"`, ""},
		{"logout-number", `1e400`, ""},
		{"logout-accepted", `null`, ""},
		{"logout-partial-content", `null`, ""},
		{"logout-explicit-false", `{"success":false,"opaque":"` + logoutCanary + `"}`, "success=false"},
		{"logout-explicit-error", `{"error":"` + logoutCanary + `"}`, "包含错误字段"},
		{"logout-html", `<html>` + logoutCanary + `</html>`, "HTML"},
		{"logout-success-large-number", `{"success":true,"vendor_number":1e400,"opaque":"` + logoutCanary + `"}`, ""},
	} {
		scenario := test.name
		t.Run(scenario, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			const email = "reset-runner@example.test"
			const oldSeed = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
			const newSeed = "JBSWY3DPEHPK3PXP"
			responseReceipt := test.logoutBody != "" || scenario == "logout-empty-body"
			unconfirmedReceipt := test.logoutReason != ""
			unknownLogout := scenario == "logout-unconfirmed" || unconfirmedReceipt
			continueWorkflow := scenario == "logout-null" || scenario == "logout-explicit-error"
			logoutHTTP := http.StatusOK
			switch scenario {
			case "logout-accepted":
				logoutHTTP = http.StatusAccepted
			case "logout-partial-content":
				logoutHTTP = http.StatusPartialContent
			}
			vault := &resetRunnerVault{MemoryStore: credentials.NewMemoryStore()}
			vault.failCommit.Store(scenario == "persistence-recovery")
			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			registerRunnerSyntheticProtection(t, cfg.OutputRoot)
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
			input := map[string]any{"resetTotp": true, "logoutAllDevicesAfterTotp": logout}
			if continueWorkflow {
				input["reloginAfterTotp"], input["uploadAfterTotp"] = true, true
				input["sub2apiConfig"] = map[string]any{"baseUrl": "https://sub2api.example.test", "adminApiKey": "fixture-upload-key", "groupIds": []string{"11"}}
			}
			job, err = manager.QueueOperation(job.ID, "reset-2fa", input)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var calls []string
			info := 0
			remoteFactor := "old-factor"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveFreshLoginFixture(t, w, r, email) {
					return
				}
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
				if r.URL.Path == "/backend-api/accounts/mfa_info" {
					if remoteFactor == "new-factor" && (scenario == "activation-unverified" || scenario == "activation-ack-read-unavailable") {
						// A lost receipt needs this read; a complete receipt must not request it.
						http.Error(w, "fixture verification unavailable", http.StatusServiceUnavailable)
						return
					}
					if remoteFactor == "" {
						switch scenario {
						case "compact-disabled":
							fmt.Fprint(w, `{"mfa_enabled_v2":false}`)
							return
						case "null-totp-disabled":
							fmt.Fprint(w, `{"mfa_enabled_v2":false,"factors":{"totp":null}}`)
							return
						}
					}
					factor(remoteFactor)
					return
				}
				switch step {
				case "/":
					if r.URL.Query().Get("action") != "enable" {
						t.Error("unexpected login request")
					}
					fmt.Fprint(w, `{"accessToken":"fixture-access-token-at-least-twenty-characters"}`)
				case "/api/auth/session":
					_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "fixture-account"}, "user": map[string]any{"email": email}})
				case "/backend-api/accounts/mfa/user/disable_in_house":
					c, _ := vault.Load(email)
					j, _ := manager.Get(job.ID)
					if !c.TOTPResetPending || !j.TOTPRecoveryPending {
						t.Error("factor removed before durable quarantine")
					}
					remoteFactor = ""
					if scenario == "compact-disabled" || scenario == "null-totp-disabled" {
						// Only an unconfirmed removal response requires a live
						// disabled-state read before enrollment.
						fmt.Fprint(w, `{}`)
						return
					}
					fmt.Fprint(w, `{"message":"Factor disabled"}`)
				case "/backend-api/accounts/mfa/enroll":
					fmt.Fprintf(w, `{"secret":%q,"session_id":"new-session","factor":{"id":"new-factor","factor_type":"totp","is_recovery":false}}`, newSeed)
				case "/backend-api/accounts/mfa/user/activate_enrollment":
					c, _ := vault.Load(email)
					if !c.TOTPResetPending || c.PendingTOTPSecret != newSeed || c.PendingTOTPFactorID != "new-factor" {
						t.Error("activation before protected pending seed")
					}
					remoteFactor = "new-factor"
					if scenario == "activation-unverified" {
						// The remote change happened, but this response does not
						// establish its result. The failed query must retain uncertainty.
						fmt.Fprint(w, `{}`)
						return
					}
					fmt.Fprint(w, `{"success":true}`)
				case "/backend-api/accounts/logout_all":
					c, _ := vault.Load(email)
					j, _ := manager.Get(job.ID)
					if c.TOTPResetPending || c.TOTPSecret != newSeed || c.PendingTOTPSecret != "" || !j.TOTPResetCommitted || !j.TOTPActivationVerified || j.TOTPResetOperationID != c.TOTPResetOperationID || j.TOTPResetPhase != "credential_persisted" {
						t.Error("logout before confirmed credential commit")
					}
					http.SetCookie(w, &http.Cookie{Name: "logout-session-rotation", Value: "fixture-logout-cookie", Path: "/", HttpOnly: true})
					if scenario == "logout-unconfirmed" {
						http.Error(w, "fixture logout unavailable", http.StatusServiceUnavailable)
						return
					}
					if responseReceipt {
						if scenario == "logout-html" {
							w.Header().Set("Content-Type", "text/html")
						}
						w.WriteHeader(logoutHTTP)
						fmt.Fprint(w, test.logoutBody)
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
			cfg.SentinelSDKURL = server.URL + "/fixture-sdk.js"
			checkpointPath := filepath.Join(cfg.OutputRoot, job.ID, "login-checkpoint.json")
			journalPath := filepath.Join(cfg.OutputRoot, job.ID, "totp-setup-result.json")
			runner := makeJobRunnerWithCredentials(cfg, manager, logger, vault, true)
			var followups []string
			if continueWorkflow {
				// Exercise the production reset HTTP path, then observe workflow
				// admission without adding another remote login or upload fixture.
				operation := jobOperationRunner{cfg: cfg, manager: manager, logger: logger, credStore: vault, durableCredentials: true}
				runner = makeTOTPWorkflowRunner(manager, vault, func(ctx context.Context, active *jobs.Job, emit func(jobs.JobEvent)) error {
					if active.Operation == "reset-2fa" {
						return operation.run(ctx, active, emit)
					}
					followups = append(followups, active.Operation)
					state, _ := manager.Get(active.ID)
					values, _ := manager.InputCopy(active.ID)
					if active.Operation != "relogin" || state.LogoutAllDevicesStatus != "succeeded" || values["totpSecret"] != newSeed || state.ResultSaved {
						t.Error("follow-up login preceded logout or reused old authorization/credentials")
					}
					manager.UpdateResult(active.ID, true, "synthetic fresh authorization saved")
					return nil
				}, func(_ context.Context, active *jobs.Job, _ func(jobs.JobEvent)) error {
					followups = append(followups, "upload")
					state, _ := manager.Get(active.ID)
					if state.TOTPReloginStatus != "succeeded" || !state.ResultSaved {
						t.Error("upload preceded saved fresh authorization")
					}
					return nil
				})
			}
			err = runner(t.Context(), job, func(event jobs.JobEvent) {
				if responseReceipt {
					manager.Emit(event)
				}
			})
			recovery := scenario == "activation-unverified" || scenario == "persistence-recovery"
			if (err != nil) != (recovery || unknownLogout) {
				t.Fatalf("unexpected result: %v", err)
			}
			if continueWorkflow {
				var wantFollowups []string
				if !unknownLogout {
					wantFollowups = []string{"relogin", "upload"}
				}
				if !reflect.DeepEqual(followups, wantFollowups) {
					t.Fatalf("logout admitted the wrong follow-up sequence: %v", followups)
				}
				state, _ := manager.Get(job.ID)
				if unknownLogout {
					if state.TOTPReloginStatus != "skipped" || state.TOTPUploadStatus != "skipped" {
						t.Fatal("unknown logout left later workflow steps runnable")
					}
				} else if state.TOTPWorkflowStatus != "succeeded" || state.TOTPReloginStatus != "succeeded" || state.TOTPUploadStatus != "succeeded" || !state.ResultSaved {
					t.Fatal("compatible logout did not complete its requested workflow")
				}
			}
			want := []string{"/", "/api/auth/session", "info1", "/backend-api/accounts/mfa/user/disable_in_house"}
			nextInfo := 2
			if scenario == "compact-disabled" || scenario == "null-totp-disabled" {
				want = append(want, "info2")
				nextInfo++
			}
			want = append(want, "/backend-api/accounts/mfa/enroll", "/backend-api/accounts/mfa/user/activate_enrollment")
			verificationReads := 0
			if scenario == "activation-unverified" {
				verificationReads = 4 // Shared budget includes each retryable 503.
			}
			for i := 0; i < verificationReads; i++ {
				want = append(want, fmt.Sprintf("info%d", nextInfo+i))
			}
			if logout && !recovery {
				want = append(want, "/backend-api/accounts/logout_all")
			}
			mu.Lock()
			got := append([]string(nil), calls...)
			mu.Unlock()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("request sequence: %v", got)
			}
			assertNoTaskCheckpoint(t, checkpointPath)
			c, _ := vault.Load(email)
			j, _ := manager.Get(job.ID)
			accountOutcomes := observationBySource(t, manager, job.ID, "totp")
			wantOutcome := "success"
			if recovery {
				wantOutcome = "failed"
			}
			if scenario == "persistence-recovery" {
				if len(accountOutcomes) != 0 {
					t.Fatal("local persistence failure mislabeled as failed account request", accountOutcomes)
				}
			} else if len(accountOutcomes) != 1 || accountOutcomes[0].Status != wantOutcome {
				t.Fatal("production reset observation missing or rejected by identity guard", accountOutcomes)
			}
			if scenario == "activation-unverified" && accountOutcomes[0].HTTPStatus != http.StatusServiceUnavailable {
				t.Fatal("production reset lost real HTTP response status", accountOutcomes)
			}
			logoutOutcomes := observationBySource(t, manager, job.ID, "logout")
			if logout && !recovery {
				wantLogout, wantHTTP := "success", http.StatusNoContent
				if scenario == "logout-unconfirmed" {
					wantLogout, wantHTTP = "failed", http.StatusServiceUnavailable
				} else if unconfirmedReceipt {
					wantLogout, wantHTTP = "failed", http.StatusOK
				} else if responseReceipt {
					wantHTTP = logoutHTTP
				}
				if len(logoutOutcomes) != 1 || logoutOutcomes[0].Status != wantLogout || logoutOutcomes[0].HTTPStatus != wantHTTP {
					t.Fatal("production optional logout outcome missing", logoutOutcomes)
				}
			} else if len(logoutOutcomes) != 0 {
				t.Fatal("unattempted logout produced an observation", logoutOutcomes)
			}
			meta, _ := os.ReadFile(filepath.Join(cfg.OutputRoot, "job-meta.json"))
			for _, secret := range []string{oldSeed, newSeed, "fixture-password", "fixture-upload-key", "otpauth://", logoutCanary} {
				if strings.Contains(string(meta), secret) {
					t.Fatal("plaintext secret in metadata")
				}
			}
			if recovery {
				if !c.TOTPResetPending || c.PendingTOTPSecret != newSeed || c.PendingTOTPFactorID != "new-factor" || !j.TOTPRecoveryPending {
					t.Fatal("unverified activated seed not preserved and blocked")
				}
				b, readErr := protocol.ReadTOTPResetJournalJSON(journalPath)
				var result map[string]any
				if readErr != nil || json.Unmarshal(b, &result) != nil || result["activation_succeeded"] != (scenario == "persistence-recovery") || result["activation_verified"] != (scenario == "persistence-recovery") {
					t.Fatal("activation evidence lost")
				}
				if scenario == "persistence-recovery" && (result["activation_confirmation_source"] != "business_receipt" || result["activation_confirmed_by_read"] == true) {
					t.Fatal("credential recovery lost the receipt or invented a list match")
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
					if rj == nil || !rj.TOTPRecoveryPending || rj.HasTOTPKey || saved.TOTPSecret != oldSeed || !saved.TOTPResetPending || saved.PendingTOTPSecret != newSeed {
						t.Fatal("startup committed verified credentials before an explicit continuation")
					}
					if _, e := os.Stat(journalPath); e != nil {
						t.Fatal("startup removed the verified recovery journal", e)
					}
					continued, queueErr := restarted.QueueOperation(job.ID, "complete-2fa", nil)
					if queueErr != nil {
						t.Fatal("verified recovery did not offer explicit local completion", queueErr)
					}
					// Local completion must not validate authentication settings or
					// initialize a transport after a durable activation receipt.
					continued.AuthorizationMethod = "obsolete-authentication-method"
					localCfg := cfg
					localCfg.TLSProfile = "unavailable-local-completion-profile"
					if err := makeJobRunnerWithCredentials(localCfg, restarted, logger, vault, true)(t.Context(), continued, func(jobs.JobEvent) {}); err != nil {
						t.Fatal("explicit verified recovery did not complete locally", err)
					}
					saved, _ = vault.Load(email)
					rj, _ = restarted.Get(job.ID)
					if rj.TOTPRecoveryPending || !rj.HasTOTPKey || saved.TOTPSecret != newSeed || saved.TOTPResetPending {
						t.Fatal("explicit local continuation did not commit verified credentials")
					}
					if completed, e := protocol.ReadTOTPResetCompletion(journalPath, email); e != nil || completed["credential_persisted"] != true {
						t.Fatal("local completion evidence was not retained", e)
					}
				} else {
					if rj == nil || !rj.TOTPRecoveryPending || rj.Status != jobs.StatusFailed {
						t.Fatal("restart failed to retain quarantine")
					}
					if _, err := restarted.Retry(job.ID); err == nil {
						t.Fatal("restart permits reset replay")
					}
					if _, err := restarted.QueueOperation(job.ID, "complete-2fa", nil); err == nil {
						t.Fatal("unknown activation admitted for local completion")
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
				if completed, readErr := protocol.ReadTOTPResetCompletion(journalPath, email); readErr != nil || completed["credential_persisted"] != true {
					t.Fatal("completed reset lacks durable activation evidence", readErr)
				}
				if !j.TOTPResetCommitted {
					t.Fatal("completed runner lacks durable metadata")
				}
				wantLogout := "not_requested"
				if logout {
					wantLogout = "succeeded"
				}
				if unknownLogout {
					wantLogout = "unknown"
				}
				if j.LogoutAllDevicesStatus != wantLogout {
					t.Fatalf("logout outcome: %s", j.LogoutAllDevicesStatus)
				}
				if unconfirmedReceipt {
					if j.TOTPWorkflowStatus != "failed" || j.ResultSaved || j.TOTPResetStatus != "activated" || j.TOTPResetPhase != "credential_persisted" || !strings.Contains(j.TOTPWorkflowError, "新 2FA 已保存") {
						t.Fatal("unconfirmed logout erased the committed factor or completed the workflow")
					}
					if !strings.Contains(j.LogoutAllDevicesError, "HTTP 200") || !strings.Contains(j.LogoutAllDevicesError, "不会自动重试") {
						t.Fatal("unconfirmed logout lost its observed HTTP status or replay boundary")
					}
					if !strings.Contains(j.LogoutAllDevicesError, test.logoutReason) {
						t.Fatalf("unconfirmed logout lost its safe response classification: %s", j.LogoutAllDevicesError)
					}
					logoutLogs := 0
					for _, entry := range j.LogEntries {
						if entry.Stage == "totp_logout_unknown" {
							logoutLogs++
							if !strings.Contains(entry.Message, "HTTP 200") || !strings.Contains(entry.Message, "不会自动重试") || !strings.Contains(entry.Message, test.logoutReason) {
								t.Fatal("persisted logout log lost its safe response diagnostic")
							}
						}
					}
					if logoutLogs != 1 {
						t.Fatal("unconfirmed logout did not retain exactly one result log")
					}
				}
				if responseReceipt {
					public, marshalErr := json.Marshal(jobs.SanitizeJobForPublic(j))
					journal, readErr := protocol.ReadTOTPResetJournalJSON(journalPath)
					if marshalErr != nil || readErr != nil || strings.Contains(string(public), logoutCanary) || strings.Contains(string(journal), logoutCanary) || err != nil && strings.Contains(err.Error(), logoutCanary) {
						t.Fatal("logout retained private response content")
					}
				}
			}
		})
	}
}
