package containercache_test

import (
	"errors"
	"os"
	"testing"

	"tosub2-go-port/internal/accountlock"
)

// Keep the image's default environment intact: the ordinary package TestMain
// isolates cache directories and would hide an unwritable production default.
// scripts/test-container.sh runs this binary as non-root with a read-only root
// and a private, disposable /data tmpfs.
func TestContainerDefaultAccountLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("container account-lock regression must run without root privileges")
	}
	const origin, email = "https://container-cache.example.invalid", "synthetic-container@example.invalid"
	release, err := accountlock.Acquire(origin, email)
	if err != nil {
		t.Fatal("default image cache did not allow an account operation", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Error("account lock cleanup failed", err)
		}
	})
	contender, err := accountlock.Acquire("https://CONTAINER-CACHE.example.invalid:443/path", "  SYNTHETIC-CONTAINER@example.invalid  ")
	if contender != nil {
		_ = contender()
	}
	if !errors.Is(err, accountlock.ErrBusy) {
		t.Fatal("equivalent account identity did not share the default lock", err)
	}
	if err := release(); err != nil {
		t.Fatal("account lock could not be released", err)
	}
	reacquired, err := accountlock.Acquire(origin, email)
	if err != nil {
		t.Fatal("released account lock prevented a new operation", err)
	}
	t.Cleanup(func() {
		if err := reacquired(); err != nil {
			t.Error("reacquired account lock cleanup failed", err)
		}
	})
}
