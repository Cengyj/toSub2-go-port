//go:build !windows

package privatefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixPrivatePermissionsRestrictOnlyOwnedDirectory(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "private")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(target); err != nil {
		t.Fatal(err)
	}
	private, _ := os.Stat(target)
	ancestor, _ := os.Stat(parent)
	if private.Mode().Perm() != 0700 || ancestor.Mode().Perm() != 0755 {
		t.Fatal("wrong directory scope or mode")
	}
	f, err := CreateTemp(target, "fixture-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	if info.Mode().Perm() != 0600 {
		t.Fatal("temporary mode must be 0600 before data is written")
	}
	if err = f.Chmod(0644); err != nil {
		t.Fatal(err)
	}
	if err = Verify(f); !errors.Is(err, ErrPermissions) {
		t.Fatal("broad file permissions were accepted")
	}
}
func TestUnixPrivateDirectoryRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	// Mkdir applies the caller's umask; set the fixture's shared mode
	// explicitly so a restrictive test runner does not look like mutation.
	if err := os.Chmod(real, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(link); !errors.Is(err, ErrPermissions) {
		t.Fatal("symlink privacy change accepted")
	}
	if err := EnsureParentDir(link); !errors.Is(err, ErrPermissions) {
		t.Fatal("symlink export parent accepted")
	}
	if f, err := CreateTemp(link, "fixture-"); !errors.Is(err, ErrPermissions) || f != nil {
		t.Fatal("temporary creation through symlink parent accepted")
	}
	info, err := os.Stat(real)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatal("symlink target permissions changed")
	}
}

func TestUnixExportParentRetainsSharedMode(t *testing.T) {
	parent := t.TempDir()
	const sharedMode = os.FileMode(0777) | os.ModeSticky
	if err := os.Chmod(parent, sharedMode); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{parent, filepath.Join(parent, "new", "nested")} {
		if err := EnsureParentDir(dir); err != nil {
			t.Fatal(err)
		}
		f, err := CreateTemp(dir, "export-")
		if err != nil {
			t.Fatal(err)
		}
		if err = Verify(f); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(parent)
	if err != nil || before.Mode() != after.Mode() {
		t.Fatal("export creation changed existing shared directory permissions")
	}
	for _, dir := range []string{filepath.Join(parent, "new"), filepath.Join(parent, "new", "nested")} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("new export directory was not private")
		}
	}
}
