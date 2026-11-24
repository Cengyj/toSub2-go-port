package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/protocol"
)

// A pending enrollment belongs to the original output root and cannot be
// replaced merely because a fresh operation uses a different journal path.
// The second timing models a peer publishing evidence before this runner
// acquires its OS account lease, after local queue admission has completed.
func TestSharedVaultPendingSetupNeverStartsReplacementEnrollment(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, operation := range []string{"setup-2fa", "setup_2fa", "setup-totp", "totp_setup", "reset-2fa"} {
		for _, pending := range sharedVaultPendingCandidates(t) {
			for _, timing := range []string{"before-queue", "after-queue"} {
				t.Run(operation+"/"+pending.name+"/"+timing, func(t *testing.T) {
					remote := &sessionAuditRemote{t: t, sessionValid: true, enrollmentExists: true}
					server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
					defer server.Close()
					cfg := sessionAuditConfig(t.TempDir(), server.URL)
					if operation == "setup-totp" {
						cfg.CLI.SetupTOTP, cfg.CLI.Email = true, sessionAuditEmail
						cfg.CLI.TOTPResult = filepath.Join(cfg.OutputRoot, "explicit-cli-setup.json")
					}
					vault := credentials.NewMemoryStore()
					manager, _, logger := sessionAuditManager(t, cfg, vault)
					sessionAuditImport(t, cfg, manager)
					saved, err := vault.Load(sessionAuditEmail)
					if err != nil {
						t.Fatal(err)
					}
					saved.PendingTOTPSecret, saved.PendingTOTPURI, saved.PendingTOTPFactorID = pending.secret, pending.uri, pending.factor
					assertEvidenceUnchanged := sharedVaultOriginalSetupEvidence(t)
					if timing == "before-queue" {
						if err := vault.Save(sessionAuditEmail, saved); err != nil {
							t.Fatal(err)
						}
					}
					_, admissionErr := manager.QueueOperation(sessionAuditJobID, operation, nil)
					if timing == "after-queue" {
						if admissionErr != nil {
							t.Fatal("clean synthetic setup could not be queued", admissionErr)
						}
						if err := vault.Save(sessionAuditEmail, saved); err != nil {
							t.Fatal(err)
						}
					}
					operationErr := admissionErr
					if admissionErr == nil {
						operationErr = sessionAuditExecute(t, cfg, manager, logger, vault)
					}
					state := remote.snapshot()
					retained, loadErr := vault.Load(sessionAuditEmail)
					if operationErr == nil || state.requests != 0 || state.loginRequests != 0 || state.enrollCalls != 0 || state.activateCalls != 0 || state.disableCalls != 0 {
						t.Errorf("new 2FA change reused an account with an unresolved enrollment: denied=%v requests=%d login=%d enroll=%d activate=%d disable=%d", operationErr != nil, state.requests, state.loginRequests, state.enrollCalls, state.activateCalls, state.disableCalls)
					}
					if loadErr != nil || !credentials.EqualReadback(saved, retained) {
						t.Error("replacement setup overwrote original pending credential material", loadErr)
					}
					assertEvidenceUnchanged()
					if admissionErr != nil && !errors.Is(admissionErr, jobs.ErrTOTPSetupVerificationPending) && !errors.Is(admissionErr, jobs.ErrTOTPRecoveryPending) {
						t.Error("setup was refused without its recovery classification", admissionErr)
					}
				})
			}
		}
	}
}

func TestSharedVaultPendingCandidateWithoutActiveSeedWaitsForManualMFA(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	vault := credentials.NewMemoryStore()
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		counts[r.URL.Path]++
		write := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		}
		switch r.URL.Path {
		case "/api/accounts/password/verify":
			write(map[string]any{"continue_url": "http://" + r.Host + "/mfa-challenge/remote-factor", "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_challenge_factors": []any{map[string]any{"factor_type": "totp", "id": "remote-factor"}}}})
		case "/api/accounts/mfa/issue_challenge":
			write(map[string]any{"success": true})
		case "/api/accounts/mfa/verify":
			t.Error("an unresolved candidate was automatically submitted without an active seed")
			w.WriteHeader(http.StatusBadRequest)
		default:
			if !serveFreshLoginFixture(t, w, r, sessionAuditEmail) {
				t.Errorf("unexpected request while awaiting manual MFA: %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}
	}))
	defer server.Close()
	cfg := sessionAuditConfig(t.TempDir(), server.URL)
	manager, _, logger := sessionAuditManager(t, cfg, vault)
	if _, err := manager.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"password": "synthetic-password"}}); err != nil {
		t.Fatal(err)
	}
	pending := sharedVaultPendingCandidates(t)[0]
	saved := credentials.Credentials{Password: "synthetic-password", PendingTOTPSecret: pending.secret, PendingTOTPURI: pending.uri, PendingTOTPFactorID: pending.factor}
	if err := vault.Save(sessionAuditEmail, saved); err != nil {
		t.Fatal(err)
	}
	assertEvidenceUnchanged := sharedVaultOriginalSetupEvidence(t)
	_, admissionErr := manager.RetryWithValues(sessionAuditJobID, nil)
	if admissionErr == nil {
		runner := makeJobRunnerWithCredentials(cfg, manager, logger, vault, true)
		waiting := make(chan struct{}, 1)
		manager.SetRunner(func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) error {
			return runner(ctx, job, func(event jobs.JobEvent) {
				emit(event)
				if event.Stage == "totp" && event.Status == jobs.StatusWaitingInput {
					select {
					case waiting <- struct{}{}:
					default:
					}
				}
			})
		})
		manager.Start()
		select {
		case <-waiting:
		case <-time.After(5 * time.Second):
			t.Fatal("accepted login did not wait for manual MFA")
		}
		mu.Lock()
		if counts["/api/auth/signin/openai"] != 1 || counts["/api/accounts/mfa/issue_challenge"] != 1 || counts["/api/accounts/mfa/verify"] != 0 {
			t.Error("login did not reach a single manual MFA challenge without automatic submission")
		}
		mu.Unlock()
		if canceled, err := manager.CancelWithError(sessionAuditJobID); err != nil || !canceled {
			t.Fatal("could not cancel manual MFA wait", err)
		}
		if err := sessionAuditWait(t, manager); err == nil {
			t.Error("cancelled manual login reported success")
		}
	} else {
		mu.Lock()
		if len(counts) != 0 {
			t.Error("rejected login made a network request")
		}
		mu.Unlock()
	}
	retained, err := vault.Load(sessionAuditEmail)
	if err != nil || !credentials.EqualReadback(saved, retained) {
		t.Error("manual MFA wait consumed unresolved candidate material", err)
	}
	assertEvidenceUnchanged()
}

type sharedVaultPendingCandidate struct{ name, secret, uri, factor string }

func sharedVaultPendingCandidates(t *testing.T) []sharedVaultPendingCandidate {
	t.Helper()
	const secret = "MFRGGZDFMZTWQ2LK"
	uri, err := protocol.BuildTOTPURI(sessionAuditEmail, secret)
	if err != nil {
		t.Fatal(err)
	}
	return []sharedVaultPendingCandidate{
		{"complete", secret, uri, "previous-root-factor"},
		{"secret-only", secret, "", ""},
		{"uri-only", "", uri, ""},
		{"factor-only", "", "", "previous-root-factor"},
	}
}

func sharedVaultOriginalSetupEvidence(t *testing.T) func() {
	t.Helper()
	pending := sharedVaultPendingCandidates(t)[0]
	root := t.TempDir()
	registerRunnerSyntheticProtection(t, root)
	path := filepath.Join(root, "original-setup", "totp-setup-result.json")
	if err := privatestate.WriteJSON(t.Context(), path, "totp-reset", sessionAuditEmail, "original-setup-operation", map[string]any{
		"version": 1, "operation_id": "original-setup-operation", "email": sessionAuditEmail, "account_id": "synthetic-account",
		"setup_evidence_schema": 1, "enrollment_dispatch": "may_have_been_sent", "activation_dispatch": "not_sent",
		"secret": pending.secret, "otpauth_uri": pending.uri, "session_id": "original-setup-session", "new_totp_factor_id": pending.factor,
		"enrollment_factor":    map[string]any{"id": pending.factor, "factor_type": "totp", "is_recovery": false},
		"enrollment_validated": true, "activation_mode": "automatic", "activation_succeeded": false, "seed_verified": false,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Error("unrelated operation changed the original protected setup journal", err)
		}
	}
}

// This test does not make unresolved candidates a new login credential. If an
// operation is accepted, only the existing active seed may be used, and every
// candidate field must survive unrelated password/MFA/OAuth credential saves.
func TestSharedVaultOrdinaryLoginNeverConsumesPendingSetupMaterial(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	for _, pending := range sharedVaultPendingCandidates(t) {
		for _, mfa := range []bool{false, true} {
			name := "password"
			if mfa {
				name = "active-mfa"
			}
			t.Run(pending.name+"/"+name, func(t *testing.T) {
				vault := credentials.NewMemoryStore()
				var mu sync.Mutex
				counts := map[string]int{}
				var state, challenge string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					counts[r.URL.Path]++
					write := func(value any) {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(value)
					}
					base := "http://" + r.Host
					switch r.URL.Path {
					case "/api/accounts/password/verify":
						if mfa {
							write(map[string]any{"continue_url": base + "/mfa-challenge/active-factor", "page": map[string]any{"type": "mfa_challenge"}, "oai-client-auth-session": map[string]any{"mfa_challenge_factors": []any{map[string]any{"factor_type": "totp", "id": "active-factor"}}}})
							return
						}
					case "/api/accounts/mfa/issue_challenge", "/api/accounts/mfa/verify":
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["id"] != "active-factor" {
							t.Error("ordinary login used an unresolved factor binding", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if r.URL.Path == "/api/accounts/mfa/verify" {
							now := time.Now()
							current, err := protocol.GenerateTOTP(sessionAuditOld, now)
							previous, previousErr := protocol.GenerateTOTP(sessionAuditOld, now.Add(-30*time.Second))
							if err != nil || previousErr != nil || body["code"] != current && body["code"] != previous {
								t.Error("ordinary login used an unverified candidate instead of the active seed")
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							write(map[string]any{"continue_url": base + "/api/auth/callback/openai?code=fixture-once"})
						} else {
							write(map[string]any{"success": true})
						}
						return
					case "/oauth/authorize":
						state, challenge = r.URL.Query().Get("state"), r.URL.Query().Get("code_challenge")
						http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?"+url.Values{"state": {state}, "code": {"shared-vault-code"}}.Encode(), http.StatusFound)
						return
					case "/oauth/token":
						_ = r.ParseForm()
						if state == "" || r.PostForm.Get("code") != "shared-vault-code" || !protocol.VerifyPKCE(r.PostForm.Get("code_verifier"), challenge) {
							t.Error("ordinary login lost its OAuth transaction binding")
						}
						identity := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"`+sessionAuditEmail+`","sub":"synthetic-user","https://api.openai.com/auth":{"chatgpt_account_id":"synthetic-account"}}`)) + ".fixture"
						write(protocol.TokenSet{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", IDToken: identity, ExpiresIn: 3600})
						return
					}
					if !serveFreshLoginFixture(t, w, r, sessionAuditEmail) {
						t.Errorf("ordinary login sent an unexpected request: %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				cfg := sessionAuditConfig(t.TempDir(), server.URL)
				manager, _, logger := sessionAuditManager(t, cfg, vault)
				// Leave the seed out of the local job so the runner must resolve
				// the active value from the shared vault itself.
				if _, err := manager.Submit(jobs.Spec{ID: sessionAuditJobID, Email: sessionAuditEmail, ImportOnly: true, Input: map[string]any{"password": "synthetic-password"}}); err != nil {
					t.Fatal(err)
				}
				// Retry must classify the new intent as ordinary authorization,
				// even if the preceding, never-started operation was a setup.
				if _, err := manager.QueueOperation(sessionAuditJobID, "setup-2fa", nil); err != nil {
					t.Fatal(err)
				}
				if canceled, err := manager.CancelWithError(sessionAuditJobID); err != nil || !canceled {
					t.Fatal("could not cancel the synthetic prior setup", err)
				}
				prior, ok := manager.Get(sessionAuditJobID)
				if !ok || prior.Operation != "setup-2fa" {
					t.Fatal("fixture lost the prior setup operation before Retry")
				}
				saved := credentials.Credentials{Password: "synthetic-password", TOTPSecret: sessionAuditOld, TOTPFactorID: "active-factor", PendingTOTPSecret: pending.secret, PendingTOTPURI: pending.uri, PendingTOTPFactorID: pending.factor}
				if err := vault.Save(sessionAuditEmail, saved); err != nil {
					t.Fatal(err)
				}
				assertEvidenceUnchanged := sharedVaultOriginalSetupEvidence(t)
				_, admissionErr := manager.RetryWithValues(sessionAuditJobID, nil)
				operationErr := admissionErr
				if admissionErr == nil {
					operationErr = sessionAuditExecute(t, cfg, manager, logger, vault)
				}
				retained, err := vault.Load(sessionAuditEmail)
				if err != nil || !credentials.EqualReadback(saved, retained) {
					t.Error("ordinary login consumed or rewrote unresolved setup material", err)
				}
				assertEvidenceUnchanged()
				mu.Lock()
				defer mu.Unlock()
				if admissionErr != nil {
					if len(counts) != 0 {
						t.Fatal("rejected ordinary login made a network request")
					}
					return
				}
				if operationErr != nil || counts["/api/auth/signin/openai"] != 1 || counts["/oauth/token"] != 1 {
					t.Error("accepted ordinary login did not complete exactly once", operationErr)
				}
				wantMFA := 0
				if mfa {
					wantMFA = 1
				}
				if counts["/api/accounts/mfa/issue_challenge"] != wantMFA || counts["/api/accounts/mfa/verify"] != wantMFA {
					t.Error("ordinary login changed MFA verification count")
				}
			})
		}
	}
}
