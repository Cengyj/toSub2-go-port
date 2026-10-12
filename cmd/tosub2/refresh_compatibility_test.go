package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"tosub2-go-port/internal/transport"
)

func refreshCompatibilityFixture(t *testing.T, path, idToken string) {
	t.Helper()
	raw := map[string]any{"type": "sub2api-data", "version": 1, "custom": "preserve", "accounts": []any{map[string]any{"name": "Existing custom name", "concurrency": 7, "priority": 0, "proxy_id": 42, "credentials": map[string]any{"email": "rotation@example.test", "access_token": "old-access", "refresh_token": "old-refresh", "id_token": idToken, "custom": "keep"}, "extra": map[string]any{"client_id": "per-account-client", "custom": "keep"}}}}
	if err := protocol.WritePrivateJSON(path, raw); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRotationSurvivesMissingOpaqueOrChangedIDToken(t *testing.T) {
	for _, idToken := range []string{"", "provider-opaque-id-token", "old.malformed.payload"} {
		t.Run(idToken, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "job", "sub2api-import-oauth.json")
			refreshCompatibilityFixture(t, path, idToken)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				r.ParseForm()
				if r.Form.Get("client_id") != "per-account-client" || r.Form.Get("refresh_token") != "old-refresh" {
					t.Error("per-account OAuth identity lost")
				}
				json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "rotated-refresh"})
			}))
			defer server.Close()
			if err := refreshExistingExport(context.Background(), transport.NewClient(nil), server.URL, "wrong-global-client", root, "job", ""); err != nil {
				t.Fatalf("successful rotation discarded: %v", err)
			}
			var result map[string]any
			b, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(b, &result) != nil {
				t.Fatal("rotated result missing")
			}
			account := result["accounts"].([]any)[0].(map[string]any)
			saved := account["credentials"].(map[string]any)
			if calls.Load() != 1 || saved["access_token"] != "new-access" || saved["refresh_token"] != "rotated-refresh" || saved["id_token"] != idToken || saved["custom"] != "keep" || account["name"] != "Existing custom name" || account["priority"] != float64(0) || account["proxy_id"] != float64(42) || result["custom"] != "preserve" {
				t.Fatal("refresh lost rotating token or unrelated metadata")
			}
		})
	}
}

func TestRefreshInvalidSourceRejectedBeforeTokenMutation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "must not be called", 500) }))
	defer server.Close()
	for _, raw := range []any{map[string]any{"type": "wrong", "accounts": []any{}}, map[string]any{"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"refresh_token": ""}}}}} {
		root := t.TempDir()
		path := filepath.Join(root, "job", "sub2api-import-oauth.json")
		if err := protocol.WritePrivateJSON(path, raw); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		if err := refreshExistingExport(context.Background(), transport.NewClient(nil), server.URL, "client", root, "job", ""); err == nil {
			t.Fatal("malformed source accepted")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid source overwritten")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid export triggered remote token mutation")
	}
}

func TestProductionReauthorizationDoesNotReadUnrelatedCheckpoint(t *testing.T) {
	testRefreshWithoutAuthenticationRuntime(t)
}

func testRefreshWithoutAuthenticationRuntime(t *testing.T) {
	for _, operation := range []string{"refresh", "regenerate", "reauthorize"} {
		t.Run(operation, func(t *testing.T) {
			clearCLISensitiveEnvironment(t)
			cfg := config.Default()
			cfg.OutputRoot = t.TempDir()
			cfg.ChatGPTBase = "http://127.0.0.1:1"
			cfg.SameProxyRetries = 0
			cfg.ProxyURLs = nil
			cfg.RequestTimeoutMS = 500
			var grants atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/" {
					w.WriteHeader(200)
					return
				}
				if r.URL.Path != "/oauth/token" {
					http.NotFound(w, r)
					return
				}
				grants.Add(1)
				json.NewEncoder(w).Encode(map[string]any{"access_token": "accepted-new-access", "refresh_token": "accepted-new-refresh"})
			}))
			defer server.Close()
			cfg.AuthBase = server.URL
			path := filepath.Join(cfg.OutputRoot, "fixture", "sub2api-import-oauth.json")
			refreshCompatibilityFixture(t, path, "opaque-id")
			checkpoint := filepath.Join(cfg.OutputRoot, "fixture", "login-checkpoint.json")
			old := []byte("incomplete unrelated browser enrollment checkpoint")
			if err := os.WriteFile(checkpoint, old, 0600); err != nil {
				t.Fatal(err)
			}
			st, err := store.New(cfg.OutputRoot)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			manager := jobs.NewManager(ctx, 1, st, nil)
			manager.SetRunner(makeJobRunnerWithCredentials(cfg, manager, slog.New(slog.NewTextHandler(io.Discard, nil)), credentials.NewMemoryStore(), false))
			job, err := manager.Submit(jobs.Spec{ID: "fixture", Email: "rotation@example.test", Operation: operation})
			if err != nil {
				t.Fatal(err)
			}
			manager.Start()
			defer manager.Shutdown(context.Background())
			for {
				j, _ := manager.Get(job.ID)
				if j.FinishedAt != nil {
					if j.Status != jobs.StatusSucceeded || !j.ResultSaved {
						t.Fatalf("refresh blocked by unrelated checkpoint: status=%s error=%s", j.Status, j.Error)
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("reauthorization did not finish")
				case <-time.After(time.Millisecond):
				}
			}
			after, err := os.ReadFile(checkpoint)
			if err != nil || !bytes.Equal(after, old) || grants.Load() != 1 {
				t.Fatal("successful grant modified unrelated checkpoint or repeated remote exchange")
			}
		})
	}
}
