package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

type offlineUnavailableVault struct{ calls atomic.Int32 }

func (s *offlineUnavailableVault) Load(string) (credentials.Credentials, error) {
	s.calls.Add(1)
	return credentials.Credentials{}, credentials.ErrUnavailable
}
func (s *offlineUnavailableVault) Save(string, credentials.Credentials) error {
	s.calls.Add(1)
	return credentials.ErrUnavailable
}
func (s *offlineUnavailableVault) Delete(string) error {
	s.calls.Add(1)
	return credentials.ErrUnavailable
}

func TestProductionSavedGrantFinalizesWithoutBrowserProxyOrCredentialService(t *testing.T) {
	for _, operation := range []string{"login", "full", "resume"} {
		for _, invalid := range []bool{false, true} {
			t.Run(operation+"-"+map[bool]string{false: "offline-success", true: "mismatched-account-rejected"}[invalid], func(t *testing.T) {
				clearCLISensitiveEnvironment(t)
				cfg := config.Default()
				cfg.OutputRoot = t.TempDir()
				cfg.AuthBase, cfg.ChatGPTBase = "http://127.0.0.1:1", "http://127.0.0.1:1"
				cfg.TLSProfile = "unavailable-retired-profile"
				const email = "offline-owner@example.test"
				cpPath := filepath.Join(cfg.OutputRoot, "offline", "login-checkpoint.json")
				claims, _ := json.Marshal(map[string]any{"email": email, "sid": "synthetic-offline-account"})
				idToken := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
				cp := protocol.Checkpoint{Version: 1, Stage: "tokens_received", Email: email, AuthBase: cfg.AuthBase, ChatGPTBase: cfg.ChatGPTBase, OAuth: map[string]any{"clientId": cfg.CodexClientID, "redirectUri": cfg.CodexRedirectURI, "state": "fixture-state", "codeVerifier": "fixture-verifier", "callbackUrl": cfg.CodexRedirectURI + "?code=consumed-code&state=fixture-state", "tokenResponse": protocol.TokenSet{AccessToken: "synthetic-offline-access", RefreshToken: "synthetic-offline-refresh", IDToken: idToken}}}
				protocol.BindTransportSession(&cp, "retired-profile", "old-browser-agent", "http://fixture:lost-password@127.0.0.1:1", true)
				if invalid {
					cp.Email = "different-owner@example.test"
				}
				if err := protocol.WriteCheckpoint(cpPath, cp); err != nil {
					t.Fatal(err)
				}
				st, err := store.New(cfg.OutputRoot)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				manager := jobs.NewManager(ctx, 1, st, nil)
				vault := &offlineUnavailableVault{}
				manager.SetRunner(makeJobRunnerWithCredentials(cfg, manager, slog.New(slog.NewTextHandler(io.Discard, nil)), vault, true))
				job, err := manager.Submit(jobs.Spec{ID: "offline", Email: email, Operation: operation})
				if err != nil {
					t.Fatal(err)
				}
				manager.RecordAuthorization(job.ID, true, "preserve existing automation evidence", map[string]bool{"password": true})
				manager.Start()
				defer manager.Shutdown(context.Background())
				var current *jobs.Job
				for {
					current, _ = manager.Get(job.ID)
					if current.FinishedAt != nil {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("offline recovery waited for an external dependency")
					case <-time.After(time.Millisecond):
					}
				}
				if vault.calls.Load() != 0 {
					t.Fatal("accepted grant depended on the credential service")
				}
				if !current.LastAuthAutomated || current.LastAuthAutomationReason != "preserve existing automation evidence" {
					t.Fatal("file recovery invented a new authentication outcome")
				}
				if invalid {
					if current.Status != jobs.StatusFailed || current.ResultSaved {
						t.Fatal("cross-account saved grant was accepted")
					}
					if _, err := os.Stat(cpPath); err != nil {
						t.Fatal("invalid saved grant was silently removed")
					}
					return
				}
				if current.Status != jobs.StatusSucceeded || !current.ResultSaved {
					t.Fatalf("offline grant recovery failed: %s", current.Error)
				}
				if _, err := os.Stat(cpPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("completed checkpoint was not removed")
				}
				data, err := os.ReadFile(filepath.Join(cfg.OutputRoot, job.ID, "sub2api-import-oauth.json"))
				var result protocol.Sub2APIOAuthExport
				if err != nil || json.Unmarshal(data, &result) != nil || len(result.Accounts) != 1 || result.Accounts[0].Credentials.RefreshToken != "synthetic-offline-refresh" {
					t.Fatal("grant was not restored to the compatible export")
				}
			})
		}
	}
}
