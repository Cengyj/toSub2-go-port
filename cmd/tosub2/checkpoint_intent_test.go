package main

import (
	"bytes"
	"context"
	"io"
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
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/store"
)

func TestCheckpointRestoreRequiresCLIIntent(t *testing.T) {
	cases := []struct {
		name string
		cli  config.CLIConfig
		op   string
		want bool
	}{
		{"console recovery", config.CLIConfig{}, "full", true},
		{"console relogin", config.CLIConfig{}, "relogin", false},
		{"output is not resume", config.CLIConfig{Email: "owner@example.test", Checkpoint: "saved.json"}, "login", false},
		{"explicit resume", config.CLIConfig{ResumeCheckpoint: "saved.json"}, "login", true},
		{"web-only ignores old login", config.CLIConfig{ResumeCheckpoint: "saved.json", WebOnly: true}, "login", false},
		{"refresh ignores unrelated checkpoint", config.CLIConfig{ResumeCheckpoint: "saved.json"}, "refresh", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.CLI = tc.cli
			if got := shouldRestoreCheckpoint(cfg, tc.op); got != tc.want {
				t.Fatalf("restore=%v expected=%v", got, tc.want)
			}
		})
	}
}

func TestProductionCLIOutputCheckpointDoesNotResumeStaleFile(t *testing.T) {
	clearCLISensitiveEnvironment(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "fixture login unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	dir := t.TempDir()
	checkpoint := filepath.Join(dir, "output-checkpoint.json")
	before := []byte("invalid old checkpoint output")
	if err := os.WriteFile(checkpoint, before, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]string{"--email=owner@example.test", "--checkpoint=" + checkpoint, "--output-root=" + filepath.Join(dir, "jobs"), "--chatgpt-base=" + srv.URL, "--auth-base=" + srv.URL, "--browser=false", "--native-http"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	logger := logging.NewWithWriter(io.Discard)
	manager := jobs.NewManager(ctx, 1, st, logger)
	job, err := manager.Submit(jobs.Spec{ID: "fresh", Email: cfg.CLI.Email, Operation: "login", Input: cliInput(cfg.CLI, filepath.Join(cfg.OutputRoot, "fresh"))})
	if err != nil {
		t.Fatal(err)
	}
	runner := makeJobRunnerWithCredentials(cfg, manager, logger, credentials.NewMemoryStore(), false)
	if err = runner(ctx, job, func(jobs.JobEvent) {}); err == nil {
		t.Fatal("fixture should reject fresh login")
	}
	if requests.Load() == 0 {
		t.Fatalf("stale output prevented fresh authentication request: %v", err)
	}
	after, readErr := os.ReadFile(checkpoint)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("failed fresh login destroyed unrelated output checkpoint")
	}
}
