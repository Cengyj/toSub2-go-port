package accountlock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAccountLockCanonicalIdentity(t *testing.T) {
	want, err := accountIdentity("https://chat.example", "account@example.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://CHAT.EXAMPLE:443/", "https://chat.example/settings", "https://chat.example/?unused=1"} {
		got, err := accountIdentity(origin, " Account@Example.Test ")
		if err != nil || got != want {
			t.Fatalf("equivalent account escaped lock namespace: %q %v", got, err)
		}
	}
	for _, origin := range []string{"", "file:///tmp/fixture", "https://user:password@chat.example"} {
		if _, err := accountIdentity(origin, "account@example.test"); err == nil {
			t.Fatal("invalid origin accepted")
		}
	}
}

func TestAccountLockSerializesAndReleases(t *testing.T) {
	dir := t.TempDir()
	release, err := acquire(dir, "service|account")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if second, err := acquire(dir, "service|account"); !errors.Is(err, ErrBusy) {
		if second != nil {
			_ = second()
		}
		t.Fatal("same account concurrently admitted", err)
	}
	other, err := acquire(dir, "service|other-account")
	if err != nil {
		t.Fatal("independent account blocked", err)
	}
	_ = other()
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal("release not idempotent", err)
	}
	again, err := acquire(dir, "service|account")
	if err != nil {
		t.Fatal(err)
	}
	_ = again()
}

func TestAccountLockProcessExit(t *testing.T) {
	if dir := os.Getenv("TOSUB2_LOCK_TEST_CHILD"); dir != "" {
		_, err := acquire(dir, "service|account")
		if errors.Is(err, ErrBusy) {
			os.Exit(22)
		}
		if err != nil {
			os.Exit(23)
		}
		// Deliberately skip all cleanup to prove OS release after process loss.
		os.Exit(0)
	}
	dir := t.TempDir()
	runChild := func(want int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestAccountLockProcessExit$", "-test.count=1")
		cmd.Env = append(os.Environ(), "TOSUB2_LOCK_TEST_CHILD="+dir)
		out, err := cmd.CombinedOutput()
		got := 0
		if err != nil {
			var exited *exec.ExitError
			if !errors.As(err, &exited) {
				t.Fatal(err)
			}
			got = exited.ExitCode()
		}
		if got != want {
			t.Fatalf("child exit=%d want=%d: %s", got, want, out)
		}
	}
	release, err := acquire(dir, "service|account")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	runChild(22)
	_ = release()
	runChild(0)
	reacquired, err := acquire(dir, "service|account")
	if err != nil {
		t.Fatal("dead process retained account lock", err)
	}
	_ = reacquired()
}

func TestAccountLockRejectsChangedPermissionsAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission/symlink fixture; Windows enforces DACLs")
	}
	dir := t.TempDir()
	release, err := acquire(dir, "service|account")
	if err != nil {
		t.Fatal(err)
	}
	_ = release()
	files, err := filepath.Glob(filepath.Join(dir, "*.lock"))
	if err != nil || len(files) != 1 {
		t.Fatal("missing lock file", err)
	}
	if err := os.Chmod(files[0], 0644); err != nil {
		t.Fatal(err)
	}
	if release, err := acquire(dir, "service|account"); err == nil {
		_ = release()
		t.Fatal("public lock file accepted")
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, files[0]); err != nil {
		t.Fatal(err)
	}
	if release, err := acquire(dir, "service|account"); err == nil {
		_ = release()
		t.Fatal("symlink accepted")
	}
}
