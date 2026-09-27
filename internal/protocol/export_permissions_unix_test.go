//go:build !windows

package protocol

import (
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/privatefile"
)

func TestPrivateExportAndCheckpointPreserveExistingParentMode(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(parent, "export.json"), filepath.Join(parent, "checkpoint.json")}
	if err := WritePrivateJSON(paths[0], map[string]any{"fixture": "private"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteCheckpoint(paths[1], Checkpoint{Version: 1, Stage: "email_verified", Email: "fixture@example.test"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = privatefile.Verify(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(parent)
	if err != nil || after.Mode() != before.Mode() {
		t.Fatal("explicit export changed its existing parent directory permissions")
	}
}
