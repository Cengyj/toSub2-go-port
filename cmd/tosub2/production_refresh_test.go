package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/transport"
)

func TestProductionPendingRefreshBypassesUnavailableDependencies(t *testing.T) {
	for _, operation := range []string{"refresh", "regenerate", "reauthorize"} {
		for _, mismatch := range []bool{false, true} {
			t.Run(operation+map[bool]string{false: "-offline", true: "-wrong-account"}[mismatch], func(t *testing.T) {
				clearCLISensitiveEnvironment(t)
				cfg := config.Default()
				cfg.OutputRoot = t.TempDir()
				cfg.AuthBase, cfg.ChatGPTBase = "http://127.0.0.1:1", "http://127.0.0.1:1"
				cfg.TLSProfile, cfg.ProxyURL = "unavailable-profile", "http://127.0.0.1:1"
				path := filepath.Join(cfg.OutputRoot, "pending-refresh", "sub2api-import-oauth.json")
				original := refreshRotationFixture(t, path)
				canceled, cancelGrant := context.WithCancel(context.Background())
				doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
					cancelGrant()
					return &transport.Response{StatusCode: 200, Body: []byte(`{"access_token":"fixture-new-access","refresh_token":"fixture-rotated-refresh"}`)}, nil
				})
				err := refreshExistingExportAt(canceled, doer, cfg.AuthBase, cfg.CodexClientID, path, path, "first@example.test")
				if !isLocalRefreshFailure(err) || !errors.Is(err, context.Canceled) {
					t.Fatal("failed to stage accepted rotation", err)
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
				email := "first@example.test"
				if mismatch {
					email = "other@example.test"
				}
				job, err := manager.Submit(jobs.Spec{ID: "pending-refresh", Email: email, Operation: operation})
				if err != nil {
					t.Fatal(err)
				}
				manager.RecordAuthorization(job.ID, true, "preserve prior authorization", map[string]bool{"password": true})
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
						t.Fatal("local refresh recovery blocked on an external dependency")
					case <-time.After(time.Millisecond):
					}
				}
				if vault.calls.Load() != 0 {
					t.Fatal("saved refresh accessed credential service")
				}
				if !current.LastAuthAutomated || current.LastAuthAutomationReason != "preserve prior authorization" {
					t.Fatal("local persistence recovery changed authorization history")
				}
				if mismatch {
					if current.Status != jobs.StatusFailed {
						t.Fatal("cross-account recovery was accepted")
					}
					if _, err := os.Stat(refreshRecoveryPath(path)); err != nil {
						t.Fatal("invalid recovery was discarded")
					}
					return
				}
				if current.Status != jobs.StatusSucceeded || !current.ResultSaved {
					t.Fatalf("recovery failed: %s", current.Error)
				}
				assertRotatedFirstAccount(t, path, original)
				if _, err := os.Stat(refreshRecoveryPath(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("completed recovery was not removed")
				}
			})
		}
	}
}
