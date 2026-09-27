package privatefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateTemporaryFileExistsEmptyAndPrivateBeforeWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private", "nested")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := CreateTemp(dir, ".checkpoint-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer os.Remove(f.Name())
	info, err := f.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatal("new temporary file is not empty")
	}
	if !strings.HasPrefix(filepath.Base(f.Name()), ".checkpoint-") || !strings.HasSuffix(f.Name(), ".tmp") {
		t.Fatal("temporary naming contract changed")
	}
	if err = Verify(f); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("synthetic-private-fixture")); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = Verify(f); err != nil {
		t.Fatal(err)
	}
}
func TestPrivateFileAtomicReplacementRetainsPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "state.json")
	if err := os.WriteFile(target, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := CreateTemp(dir, ".replacement-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err = f.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(f.Name(), target); err != nil {
		t.Fatal(err)
	}
	result, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	if err = Verify(result); err != nil {
		t.Fatal("private permissions lost on replacement:", err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "new" {
		t.Fatal("atomic replacement failed")
	}
}
func TestPrivateFileCreationFailureDoesNotLeaveOutput(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if f, err := CreateTemp(missing, "fixture-"); err == nil || f != nil || !errors.Is(err, ErrPermissions) {
		t.Fatal("missing directory silently accepted")
	}
	for _, pattern := range []string{"../escape", "path/file", "path\\file"} {
		if f, err := CreateTemp(dir, pattern); err == nil || f != nil {
			t.Fatal("unsafe filename pattern accepted")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed private file creation left data")
	}
	if err := Verify(nil); !errors.Is(err, ErrPermissions) {
		t.Fatal("nil file privacy validation succeeded")
	}
	f, err := CreateTemp(dir, "closed-")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err = Verify(f); !errors.Is(err, ErrPermissions) {
		t.Fatal("closed handle privacy validation succeeded")
	}
}
func TestEnsureDirRejectsFilesystemRootAndFile(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	if err := EnsureDir(root); !errors.Is(err, ErrPermissions) {
		t.Fatal("filesystem root privacy modification accepted")
	}
	if err := EnsureParentDir(root); !errors.Is(err, ErrPermissions) {
		t.Fatal("filesystem root export parent accepted")
	}
	if f, err := CreateTemp(root, "fixture-"); !errors.Is(err, ErrPermissions) || f != nil {
		t.Fatal("filesystem root temporary creation accepted")
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(path); !errors.Is(err, ErrPermissions) {
		t.Fatal("file treated as directory")
	}
	if err := EnsureParentDir(path); !errors.Is(err, ErrPermissions) {
		t.Fatal("file treated as export parent")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "old" {
		t.Fatal("invalid private directory changed existing data")
	}
}
