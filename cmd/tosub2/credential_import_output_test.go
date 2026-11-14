package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type failedImportOutput struct{}

func (failedImportOutput) Write([]byte) (int, error) {
	return 0, errors.New("synthetic private writer detail")
}

func TestOfflineImportOutputFailurePreservesCancellation(t *testing.T) {
	cfg, _ := credentialImportFixture(t, "owner@example.test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handled, err := runCredentialImportWithDependencies(ctx, cfg, failedImportOutput{}, credentialImportDependencies{})
	if !handled || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "counters could not be written") {
		t.Fatalf("output failure masked the import result: %v", err)
	}
	if strings.Contains(err.Error(), "synthetic private") {
		t.Fatal("writer internals leaked into offline diagnostics")
	}
}
