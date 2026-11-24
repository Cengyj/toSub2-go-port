package accountlock

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirectoryIdentityKeepsMissingAncestorAliasesStable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode; native alias acceptance is separate")
	}
	parent := t.TempDir()
	alias := filepath.Join(t.TempDir(), "parent-alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	root, aliased := filepath.Join(parent, "new", "data"), filepath.Join(alias, "new", "data")
	before, err := DirectoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DirectoryIdentity(aliased); err != nil || got != before {
		t.Fatal("missing root's ancestor alias escaped its identity", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("identity lookup created the root", err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, aliased, resolved} {
		got, err := DirectoryIdentity(path)
		if err != nil || got != before {
			t.Fatal("directory creation changed the original lock identity", err)
		}
	}
	if other, err := DirectoryIdentity(filepath.Join(parent, "other")); err != nil || other == before {
		t.Fatal("independent directory inherited another lock identity", err)
	}
}

func TestDirectoryIdentityRejectsUnresolvableRoots(t *testing.T) {
	if _, err := DirectoryIdentity(""); err == nil {
		t.Fatal("empty root accepted")
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows developer mode; native alias acceptance is separate")
	}
	root := filepath.Join(t.TempDir(), "cycle")
	if err := os.Symlink(root, root); err != nil {
		t.Fatal(err)
	}
	if _, err := DirectoryIdentity(filepath.Join(root, "data")); err == nil {
		t.Fatal("unresolvable ancestor accepted")
	}
}
