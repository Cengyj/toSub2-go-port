package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func assertNoTaskCheckpoint(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("operation persisted login state", err)
	}
}

func assertNoAccountSessions(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, "account-sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("operation created or depended on global Cookie persistence", err)
	}
}
