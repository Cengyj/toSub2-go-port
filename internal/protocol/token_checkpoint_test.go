package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/transport"
)

// The helper is a separate OS process so only legacy JSON files survive the
// first failed export. A new Flow in the same process would not prove that.
func TestOAuthGrantRecoveryProcessHelper(t *testing.T) {
	if os.Getenv("TOSUB2_PROTOCOL_RECOVERY_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	root, base := os.Getenv("TOSUB2_PROTOCOL_RECOVERY_ROOT"), os.Getenv("TOSUB2_PROTOCOL_RECOVERY_BASE")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := NewFlow(NewClient(nil, nil, ""), FlowOptions{Email: "fixture@example.test", AuthBase: base, ChatGPTBase: base,
		CheckpointPath: filepath.Join(root, "login-checkpoint.json"), OutputPath: filepath.Join(root, "sub2api-import-oauth.json")})
	_, err := f.Run(ctx)
	if os.Getenv("TOSUB2_PROTOCOL_RECOVERY_EXPECT_FAILURE") == "1" {
		if err == nil {
			t.Fatal("export to an existing directory unexpectedly succeeded")
		}
	} else if err != nil {
		t.Fatalf("saved OAuth grant did not finish in a new process: %v", err)
	}
}

func TestLegacyCallbackCheckpointSurvivesExportFailureAcrossProcesses(t *testing.T) {
	var exchanges atomic.Int32
	jwt := base64JWT("{\"email\":\"fixture@example.test\",\"sid\":\"fixture-account\",\"sub\":\"fixture-user\"}")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			t.Error("recovery attempted an unnecessary login or OAuth request")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if exchanges.Add(1) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "legacy-one-use-code" || r.PostForm.Get("code_verifier") != "legacy-pkce-verifier" || r.PostForm.Get("redirect_uri") != DefaultCodexRedirectURI || r.PostForm.Get("client_id") != DefaultCodexClientID {
			t.Error("legacy checkpoint PKCE grant changed")
		}
		if cookie, err := r.Cookie("fixture-session"); err != nil || cookie.Value != "saved-cookie" {
			t.Error("legacy checkpoint lost its cookie jar")
		}
		json.NewEncoder(w).Encode(TokenSet{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: jwt})
	}))
	defer server.Close()
	root := t.TempDir()
	checkpointPath := filepath.Join(root, "login-checkpoint.json")
	// This is the legacy writer's v1 shape, before Go's new grant checkpoint.
	legacy := map[string]any{"version": 1, "stage": "callback_ready", "updated_at": "2026-01-01T00:00:00Z",
		"email": "fixture@example.test", "auth_base": server.URL, "chatgpt_base": server.URL,
		"cookies":          []any{map[string]any{"name": "fixture-session", "value": "saved-cookie", "domain": "127.0.0.1", "path": "/", "hostOnly": true}},
		"web":              map[string]any{"deviceId": "legacy-device", "emailVerified": true},
		"oauth":            map[string]any{"state": "legacy-state", "codeVerifier": "legacy-pkce-verifier", "workspaceId": "legacy-workspace", "callbackUrl": DefaultCodexRedirectURI + "?code=legacy-one-use-code&state=legacy-state"},
		"future_extension": map[string]any{"retained": true}}
	if err := WritePrivateJSON(checkpointPath, legacy); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(root, "sub2api-import-oauth.json")
	if err := os.Mkdir(outputPath, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(expectFailure bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOAuthGrantRecoveryProcessHelper$")
		cmd.Env = append(os.Environ(), "TOSUB2_PROTOCOL_RECOVERY_HELPER=1", "TOSUB2_PROTOCOL_RECOVERY_ROOT="+root, "TOSUB2_PROTOCOL_RECOVERY_BASE="+server.URL, "TOSUB2_PROTOCOL_RECOVERY_EXPECT_FAILURE=0")
		if expectFailure {
			cmd.Env[len(cmd.Env)-1] = "TOSUB2_PROTOCOL_RECOVERY_EXPECT_FAILURE=1"
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("recovery subprocess failed: %v\n%s", err, out)
		}
	}
	run(true)
	if exchanges.Load() != 1 {
		t.Fatal("first process did not redeem the saved grant exactly once")
	}
	saved, err := ReadCheckpoint(checkpointPath)
	if err != nil || saved.Extra["future_extension"] == nil || saved.Web["deviceId"] != "legacy-device" {
		t.Fatal("failed export discarded the saved authentication state", err)
	}
	file, err := os.Open(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	err = privatefile.Verify(file)
	file.Close()
	if err != nil {
		t.Fatal("grant checkpoint is not private", err)
	}
	if err := os.Remove(outputPath); err != nil {
		t.Fatal(err)
	}
	run(false)
	if exchanges.Load() != 1 {
		t.Fatal("second process replayed a consumed authorization code")
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var output Sub2APIOAuthExport
	if json.Unmarshal(data, &output) != nil || len(output.Accounts) != 1 || output.Type != "sub2api-data" || output.Version != 1 {
		t.Fatal("recovery did not produce the legacy import shape")
	}
	got := output.Accounts[0].Credentials
	if got.AccessToken != "fixture-access" || got.RefreshToken != "fixture-refresh" || got.IDToken != jwt || got.Email != "fixture@example.test" || got.ChatGPTAccountID != "fixture-account" {
		t.Fatal("recovery changed the accepted grant or account identity")
	}
	if _, err = os.Stat(checkpointPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed recovery did not remove its private checkpoint", err)
	}
}

func TestSavedOAuthGrantValidationNeverReplaysAuthentication(t *testing.T) {
	for _, field := range []string{"clientId", "redirectUri", "state", "codeVerifier", "tokenResponse"} {
		t.Run(field, func(t *testing.T) {
			var calls int
			f := NewFlow(NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return nil, errors.New("network must not be used for a saved grant")
			}), nil, ""), FlowOptions{Email: "fixture@example.test"})
			cp := Checkpoint{Version: 1, Stage: "tokens_received", Email: f.Options.Email, Cookies: []transport.Cookie{}, OAuth: map[string]any{
				"clientId": DefaultCodexClientID, "redirectUri": DefaultCodexRedirectURI, "state": "saved-state", "codeVerifier": "saved-verifier",
				"callbackUrl":   DefaultCodexRedirectURI + "?code=consumed-code&state=saved-state",
				"tokenResponse": TokenSet{AccessToken: "fixture-private-access", RefreshToken: "fixture-private-refresh", IDToken: "fixture-private-id"},
			}}
			cp.OAuth[field] = "fixture-private-invalid-value"
			if field == "codeVerifier" {
				cp.OAuth[field] = ""
			}
			if err := f.Restore(cp); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Run(context.Background()); err == nil || calls != 0 || strings.Contains(err.Error(), "fixture-private") {
				t.Fatal("invalid saved grant was replayed, accepted or disclosed")
			}
		})
	}
}

func TestOAuthGrantSurvivesCancellationAfterExchange(t *testing.T) {
	root := t.TempDir()
	checkpointPath := filepath.Join(root, "login-checkpoint.json")
	outputPath := filepath.Join(root, "sub2api-import-oauth.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var exchanges int
	tokens := TokenSet{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: base64JWT("{\"email\":\"fixture@example.test\",\"sid\":\"fixture-account\"}")}
	f := NewFlow(NewClient(doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		if !strings.HasSuffix(r.URL, "/oauth/token") {
			t.Fatal("callback recovery restarted authentication")
		}
		exchanges++
		// A response accepted by the remote server can race with cancellation.
		cancel()
		b, err := json.Marshal(tokens)
		if err != nil {
			t.Fatal(err)
		}
		return response(http.StatusOK, string(b)), nil
	}), nil, ""), FlowOptions{Email: "fixture@example.test", CheckpointPath: checkpointPath, OutputPath: outputPath})
	cp := Checkpoint{Version: 1, Stage: "callback_ready", Email: f.Options.Email, Cookies: []transport.Cookie{}, OAuth: map[string]any{
		"state": "fixture-state", "codeVerifier": "fixture-verifier", "callbackUrl": DefaultCodexRedirectURI + "?code=one-use-code&state=fixture-state",
	}}
	if err := f.Restore(cp); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("successful exchange did not preserve cancellation", err)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled export unexpectedly completed", err)
	}
	saved, err := ReadCheckpoint(checkpointPath)
	if err != nil || saved.Stage != tokenCheckpointStage {
		t.Fatal("accepted grant was not saved before honoring cancellation", err)
	}
	resumed := NewFlow(f.Client, f.Options)
	if _, err := resumed.Run(context.Background()); err != nil {
		t.Fatal("saved grant did not recover after cancellation", err)
	}
	if exchanges != 1 {
		t.Fatal("cancellation recovery replayed the consumed code")
	}
}

func TestSavedOAuthGrantKeepsLegacySessionShape(t *testing.T) {
	for _, mode := range []string{"session", "both"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			options := FlowOptions{Email: "fixture@example.test", OutputMode: mode, CheckpointPath: filepath.Join(root, "login-checkpoint.json"),
				OutputPath: filepath.Join(root, "sub2api-import-oauth.json"), SessionOutputPath: filepath.Join(root, "session.json")}
			f := NewFlow(NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
				t.Fatal("saved grant recovery issued a network request")
				return nil, errors.New("unexpected request")
			}), nil, ""), options)
			oauth := map[string]any{"state": "fixture-state", "codeVerifier": "fixture-verifier", "workspaceId": "fixture-workspace",
				"callbackUrl": DefaultCodexRedirectURI + "?code=consumed-code&state=fixture-state", "future_extension": true}
			tokens := TokenSet{AccessToken: "fixture-private-access", RefreshToken: "fixture-private-refresh", IDToken: base64JWT("{\"email\":\"fixture@example.test\",\"sid\":\"fixture-account\"}")}
			if err := f.saveTokenResponse(oauth, tokens); err != nil {
				t.Fatal(err)
			}
			// A canceled restart must not create a session file or discard tokens.
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := NewFlow(f.Client, options).Run(canceled); !errors.Is(err, context.Canceled) {
				t.Fatal("saved grant ignored cancellation", err)
			}
			if _, err := os.Stat(options.SessionOutputPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("canceled grant recovery wrote session output", err)
			}
			result, err := NewFlow(f.Client, options).Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(options.SessionOutputPath)
			if err != nil {
				t.Fatal(err)
			}
			var session struct {
				Codex map[string]any `json:"codex"`
			}
			if json.Unmarshal(data, &session) != nil || session.Codex["future_extension"] != true || session.Codex["workspaceId"] != "fixture-workspace" {
				t.Fatal("saved grant changed the legacy session shape")
			}
			for _, field := range []string{"tokenResponse", "clientId", "redirectUri"} {
				if session.Codex[field] != nil || result.OAuth[field] != nil {
					t.Fatal("checkpoint-only grant data leaked into the legacy OAuth object")
				}
			}
			if strings.Contains(string(data), "fixture-private") {
				t.Fatal("grant tokens leaked into legacy session output")
			}
			if mode == "session" {
				if _, err := ReadCheckpoint(options.CheckpointPath); err != nil {
					t.Fatal("session-only recovery lost the grant for later export", err)
				}
			} else if _, err := os.Stat(options.CheckpointPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed export retained its private checkpoint", err)
			}
		})
	}
}

func TestFinalizeSavedOAuthGrantValidatesAccountAndTarget(t *testing.T) {
	for _, scenario := range []string{"saved-browser-and-proxy-unavailable", "different-job-email", "different-auth-origin", "different-chat-origin",
		"missing-auth-origin", "missing-chat-origin", "different-client", "different-redirect", "different-token-email", "invalid-id-token",
		"web-only", "wrong-stage", "missing-job-email"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			options := FlowOptions{Email: "fixture@example.test", AuthBase: "https://auth.example.test", ChatGPTBase: "https://chat.example.test",
				CheckpointPath: filepath.Join(root, "login-checkpoint.json"), OutputPath: filepath.Join(root, "sub2api-import-oauth.json")}
			f := NewFlow(nil, options)
			oauth := map[string]any{"state": "fixture-state", "codeVerifier": "fixture-verifier", "callbackUrl": DefaultCodexRedirectURI + "?code=consumed-code&state=fixture-state"}
			tokenEmail := "FIXTURE@example.test"
			if scenario == "different-token-email" {
				tokenEmail = "someone-else@example.test"
			}
			claims, err := json.Marshal(map[string]any{"email": tokenEmail, "sid": "fixture-account"})
			if err != nil {
				t.Fatal(err)
			}
			tokens := TokenSet{AccessToken: "fixture-private-access", RefreshToken: "fixture-private-refresh", IDToken: base64JWT(string(claims))}
			if scenario == "invalid-id-token" {
				tokens.IDToken = "fixture-private-invalid-id"
			}
			if err := f.saveTokenResponse(oauth, tokens); err != nil {
				t.Fatal(err)
			}
			cp, err := ReadCheckpoint(options.CheckpointPath)
			if err != nil {
				t.Fatal(err)
			}
			// Bound browser and proxy material are unnecessary once token exchange
			// has completed; this offline API must not validate or recreate them.
			BindTransportSession(&cp, "retired-browser-profile", "retired-browser-agent", "http://fixture:lost-proxy-secret@unavailable.invalid", true)
			switch scenario {
			case "different-job-email":
				options.Email = "someone-else@example.test"
			case "different-auth-origin":
				options.AuthBase = "https://different-auth.example.test"
			case "different-chat-origin":
				options.ChatGPTBase = "https://different-chat.example.test"
			case "missing-auth-origin":
				cp.AuthBase = ""
			case "missing-chat-origin":
				cp.ChatGPTBase = ""
			case "different-client":
				options.ClientID = "different-client"
			case "different-redirect":
				options.RedirectURI = "http://localhost:3456/auth/callback"
			case "web-only":
				options.WebOnly = true
			case "wrong-stage":
				cp.Stage = "callback_ready"
			case "missing-job-email":
				options.Email = ""
			}
			result, err := FinalizeSavedOAuthGrant(context.Background(), cp, options)
			if scenario == "saved-browser-and-proxy-unavailable" {
				if err != nil || len(result.Export.Accounts) != 1 {
					t.Fatal("offline recovery required the lost browser/proxy session", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "CHECKPOINT_INVALID") || strings.Contains(err.Error(), "fixture-private") {
				t.Fatal("invalid saved grant was accepted or disclosed")
			}
			if _, err := os.Stat(options.OutputPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid grant created an export", err)
			}
			if _, err := ReadCheckpoint(options.CheckpointPath); err != nil {
				t.Fatal("invalid grant discarded its private recovery data", err)
			}
		})
	}
}
