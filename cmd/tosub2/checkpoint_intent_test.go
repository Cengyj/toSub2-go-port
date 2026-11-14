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

func TestCLIRejectsRemovedLoginPersistenceInputs(t *testing.T) {
	for _, flag := range []string{"--checkpoint=old.json", "--resume-checkpoint=old.json", "--out=old.json", "--web-only", "--output-mode=session"} {
		if cfg, err := config.Parse([]string{"--email=owner@example.test", flag}); err == nil && cfg.CLI.Validate() == nil {
			t.Fatalf("removed input accepted: %s", flag)
		}
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
	cfg, err := config.Parse([]string{"--email=owner@example.test", "--output-root=" + filepath.Join(dir, "jobs"), "--chatgpt-base=" + srv.URL, "--auth-base=" + srv.URL, "--native-http"})
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
