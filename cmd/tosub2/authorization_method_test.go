package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

func TestBPSOnlyChangesOAuthOperationsAndPreflight(t *testing.T) {
	for _, op := range []string{"login", "full", "resume", "relogin", "regenerate", "reset-2fa", "setup-totp", "add-password"} {
		method, err := operationAuthorizationMethod("bps", op)
		want := "bps"
		if op == "reset-2fa" || op == "setup-totp" || op == "add-password" {
			want = "chatgpt"
		}
		if err != nil || method != want {
			t.Fatalf("%s selected %s: %v", op, method, err)
		}
	}
	cfg := config.Default()
	probe := authorizationProbeConfig(cfg, "bps")
	if probe.ChatGPTBase != cfg.AuthBase || cfg.ChatGPTBase == cfg.AuthBase || authorizationProbeConfig(cfg, "chatgpt").ChatGPTBase != cfg.ChatGPTBase {
		t.Fatal("provider preflight changed shared configuration or probes ChatGPT for BPS")
	}
}

func TestCLIAuthorizationMethodInheritsSavedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	for _, saved := range []string{"", "bps"} {
		if err := protocol.WriteCheckpoint(path, protocol.Checkpoint{Version: 1, Stage: "callback_ready", Email: "owner@example.test", AuthorizationMethod: saved}); err != nil {
			t.Fatal(err)
		}
		want, opposite := "chatgpt", "bps"
		if saved == "bps" {
			want, opposite = "bps", "chatgpt"
		}
		if got, err := cliAuthorizationMethod(config.CLIConfig{ResumeCheckpoint: path}); err != nil || got != want {
			t.Fatalf("resume inferred %s: %v", got, err)
		}
		if _, err := cliAuthorizationMethod(config.CLIConfig{ResumeCheckpoint: path, AuthorizationMethod: opposite}); err == nil {
			t.Fatal("explicit method silently changed a saved grant")
		}
	}
	for _, args := range [][]string{
		{"--email=owner@example.test", "--authorization-method=invalid"},
		{"--email=owner@example.test", "--authorization-method=bps", "--web-only"},
		{"--email=owner@example.test", "--authorization-method=bps", "--setup-totp"},
	} {
		if _, err := config.Parse(args); err == nil {
			t.Fatal("unsupported CLI method combination was accepted")
		}
	}
}

func TestBPSRefreshPreservesClientAndRejectsConflictingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "import.json")
	doc := map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{map[string]any{
		"credentials": map[string]any{"email": "owner@example.test", "client_id": protocol.DefaultBPSClientID, "refresh_token": "fixture-refresh"},
		"extra":       map[string]any{},
	}}}
	if err := protocol.WritePrivateJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	if got, err := cliAuthorizationMethod(config.CLIConfig{RefreshSub2API: path}); err != nil || got != "bps" {
		t.Fatalf("refresh inferred %s: %v", got, err)
	}
	if !refreshUsesDifferentMethod(path, "chatgpt", protocol.DefaultCodexClientID) || refreshUsesDifferentMethod(path, "bps", protocol.DefaultCodexClientID) {
		t.Fatal("reauthorization can refresh a different provider's export")
	}
	account := doc["accounts"].([]any)[0].(map[string]any)
	account["extra"].(map[string]any)["client_id"] = protocol.DefaultCodexClientID
	if _, _, _, err := refreshSourceIdentity(doc, protocol.DefaultCodexClientID); err == nil {
		t.Fatal("conflicting OAuth clients accepted")
	}
}

func TestProductionBPSGrantFinalizesOfflineWithSelectedClient(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	cfg := config.Default()
	cfg.OutputRoot = t.TempDir()
	cfg.AuthBase, cfg.ChatGPTBase = "http://127.0.0.1:1", "http://127.0.0.1:2"
	cfg.TLSProfile = "unavailable-profile"
	const email = "owner@example.test"
	access := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"auth":{"chatgpt_account_id":"fixture-account"},"https://api.openai.com/profile":{"email":"owner@example.test"}}`)) + ".fixture"
	cp := protocol.Checkpoint{Version: 1, Stage: "tokens_received", AuthorizationMethod: "bps", Email: email, AuthBase: cfg.AuthBase, ChatGPTBase: cfg.ChatGPTBase, OAuth: map[string]any{
		"clientId": protocol.DefaultBPSClientID, "redirectUri": protocol.DefaultBPSRedirectURI,
		"state": "bps.fixture.PC", "codeVerifier": "fixture-verifier", "callbackUrl": protocol.DefaultBPSRedirectURI + "?code=consumed-fixture&state=bps.fixture.PC",
		"tokenResponse": protocol.TokenSet{AccessToken: access, RefreshToken: "fixture-refresh"},
	}}
	cpPath := filepath.Join(cfg.OutputRoot, "bps-offline", "login-checkpoint.json")
	if err := protocol.WriteCheckpoint(cpPath, cp); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	manager := jobs.NewManager(context.Background(), 1, st, nil)
	defer manager.Shutdown(context.Background())
	job, err := manager.Submit(jobs.Spec{ID: "bps-offline", Email: email, Operation: "resume", Input: map[string]any{"authorizationMethod": "bps"}})
	if err != nil {
		t.Fatal(err)
	}
	vault := &offlineUnavailableVault{}
	runner := makeJobRunnerWithCredentials(cfg, manager, slog.New(slog.NewTextHandler(io.Discard, nil)), vault, true)
	if err := runner(context.Background(), job, func(jobs.JobEvent) {}); err != nil {
		t.Fatal(err)
	}
	if vault.calls.Load() != 0 {
		t.Fatal("saved BPS grant accessed credentials")
	}
	data, err := os.ReadFile(filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json"))
	var result protocol.Sub2APIOAuthExport
	if err != nil || json.Unmarshal(data, &result) != nil || len(result.Accounts) != 1 || result.Accounts[0].Extra["client_id"] != protocol.DefaultBPSClientID || result.Accounts[0].Credentials.IDToken != "" {
		t.Fatal("saved BPS grant exported using a different client or fabricated ID token")
	}
}
