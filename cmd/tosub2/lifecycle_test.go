package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
)

func lifecycleConfig(t *testing.T) config.Config {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := config.Default()
	cfg.Host, cfg.Port, cfg.OutputRoot = "127.0.0.1", port, t.TempDir()
	cfg.ShutdownTimeoutMS = 2000
	cfg.StartupToken = "fixture-lifecycle-token"
	return cfg
}

func TestConsoleFailedRestorePreservesMetadataAndClosesListener(t *testing.T) {
	cfg := lifecycleConfig(t)
	path := filepath.Join(cfg.OutputRoot, "job-meta.json")
	original := []byte("{\"jobs\": [unfinished")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	ready := false
	err := runConsole(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { ready = true })
	if err == nil || !strings.Contains(err.Error(), "restore") || ready {
		t.Fatalf("invalid restore entered service: ready=%v err=%v", ready, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("failed restore overwrote original metadata")
	}
	listener, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		t.Fatalf("failed restore leaked listener: %v", err)
	}
	listener.Close()
}

func TestConsoleCancellationWaitsForMetadataAndClosesHTTP(t *testing.T) {
	cfg := lifecycleConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- runConsole(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("startup failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup timed out")
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + cfg.Addr() + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	var health struct{ OK bool }
	decodeErr := json.NewDecoder(response.Body).Decode(&health)
	response.Body.Close()
	if decodeErr != nil || !health.OK {
		t.Fatal("health endpoint did not return its JSON contract")
	}
	if response.StatusCode != 200 {
		t.Fatalf("health status=%d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("shutdown timed out")
	}
	data, err := os.ReadFile(filepath.Join(cfg.OutputRoot, "job-meta.json"))
	if err != nil || !json.Valid(data) {
		t.Fatalf("shutdown failed to persist valid metadata: %v", err)
	}
	if response, err := client.Get("http://" + cfg.Addr() + "/api/health"); err == nil {
		response.Body.Close()
		t.Fatal("HTTP listener remains open after shutdown")
	}
}
