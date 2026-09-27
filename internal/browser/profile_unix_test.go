//go:build !windows

package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChromiumProfileDirectoryIsPrivateWithoutChangingParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", parent)
	dir, err := makeProfileDirectory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeProfileDirectory(dir); err != nil {
			t.Error(err)
		}
	})
	if filepath.Dir(dir) != parent {
		t.Fatal("profile created outside configured temp parent")
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("browser profile is not private: %v %v", st, err)
	}
	st, err = os.Stat(parent)
	if err != nil || st.Mode().Perm() != 0755 {
		t.Fatalf("browser changed parent permissions: %v %v", st, err)
	}
}
