package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestBuildLockProcessHelper(t *testing.T) {
	root := os.Getenv("TOSUB2_BUILD_LOCK_FIXTURE")
	if root == "" {
		t.Skip("subprocess fixture only")
	}
	release, err := acquireBuildLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Println("build lock held")
	// The owning test kills this process to exercise OS cleanup.
	time.Sleep(time.Minute)
}

func TestBuildLockPreventsBuildAndCheckUntilKilledOwnerExits(t *testing.T) {
	root := buildFixture(t)
	if err := buildWeb(root); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBuildLockProcessHelper$")
	cmd.Env = append(os.Environ(), "TOSUB2_BUILD_LOCK_FIXTURE="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "build lock held" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("lock owner exited before acquiring lock")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lock owner did not become ready")
	}
	for _, check := range []bool{false, true} {
		if err := buildWebMode(root, check); !errors.Is(err, errBuildBusy) {
			t.Fatalf("concurrent build/check accepted: %v", err)
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := buildWebMode(root, true); err != nil {
		t.Fatalf("lock leaked or competing build altered outputs: %v", err)
	}
}
