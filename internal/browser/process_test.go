package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// This fixture uses the Go test executable itself, so it exercises process
// ownership on every target without a shell, Python, Node, or installed Chrome.
func TestProcessTreeFixtureHelper(t *testing.T) {
	role := os.Getenv("TOSUB2_PROCESS_FIXTURE_ROLE")
	if role == "" {
		t.Skip("dedicated process-tree fixture entry; exercised by lifecycle tests in subprocesses")
	}
	dir := os.Getenv("TOSUB2_PROCESS_FIXTURE_DIR")
	if role == "root" || role == "child" {
		childRole := "child"
		if role == "child" {
			childRole = "grandchild"
		}
		child := fixtureCommand(context.Background(), childRole, dir)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		defer child.Process.Kill()
	}
	if err := os.WriteFile(filepath.Join(dir, role+".pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(3)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if role == "root" {
			if _, err := os.Stat(filepath.Join(dir, "exit-parent")); err == nil {
				// Simulate a browser that unexpectedly exits without waiting for
				// children. The owning Session must still terminate descendants.
				os.Exit(0)
			}
		}
		f, err := os.OpenFile(filepath.Join(dir, role+".heartbeat"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(4)
		}
		_, _ = f.Write([]byte{1})
		_ = f.Close()
		time.Sleep(15 * time.Millisecond)
	}
}

func fixtureCommand(ctx context.Context, role, directory string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessTreeFixtureHelper$")
	cmd.Env = append(os.Environ(), "TOSUB2_PROCESS_FIXTURE_ROLE="+role, "TOSUB2_PROCESS_FIXTURE_DIR="+directory)
	return cmd
}

func waitForFixtureFiles(t *testing.T, dir string, roles ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready := true
		for _, role := range roles {
			st, err := os.Stat(filepath.Join(dir, role+".heartbeat"))
			ready = ready && err == nil && st != nil && st.Size() > 0
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("process-tree fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startTreeFixture(t *testing.T, parent context.Context) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	profile, err := makeProfileDirectory()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	cmd := fixtureCommand(ctx, "root", dir)
	tree, err := prepareProcessTree(cmd)
	if err != nil {
		cancel()
		_ = removeProfileDirectory(profile)
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = tree.terminate(nil)
		_ = removeProfileDirectory(profile)
		t.Fatal(err)
	}
	if err = tree.attach(cmd.Process); err != nil {
		cancel()
		_ = tree.terminate(cmd.Process)
		_ = cmd.Wait()
		_ = removeProfileDirectory(profile)
		t.Fatal(err)
	}
	s := &Session{cmd: cmd, tree: tree, cancel: cancel, Dir: profile, processDone: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(s.processDone); _ = s.Close() }()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	waitForFixtureFiles(t, dir, "child", "grandchild")
	return s, dir
}

func TestProcessTreeCancellationAndParentExitCleanDescendants(t *testing.T) {
	for _, mode := range []string{"cancel", "parent-exit", "explicit-close"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, dir := startTreeFixture(t, ctx)
			// A separately started process with the same executable must survive.
			otherDir := t.TempDir()
			other := fixtureCommand(context.Background(), "unrelated", otherDir)
			if err := other.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
			waitForFixtureFiles(t, otherDir, "unrelated")
			switch mode {
			case "cancel":
				cancel()
			case "parent-exit":
				if err := os.WriteFile(filepath.Join(dir, "exit-parent"), []byte{1}, 0600); err != nil {
					t.Fatal(err)
				}
			case "explicit-close":
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-s.processDone:
			case <-time.After(5 * time.Second):
				t.Fatal("parent process survived shutdown")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
				t.Fatal("profile survived cleanup")
			}
			// Give an in-flight pre-kill write time to settle, then ensure neither
			// child nor grandchild executes while the unrelated process continues.
			time.Sleep(60 * time.Millisecond)
			sizes := map[string]int64{}
			for _, role := range []string{"child", "grandchild"} {
				st, err := os.Stat(filepath.Join(dir, role+".heartbeat"))
				if err != nil {
					t.Fatal(err)
				}
				sizes[role] = st.Size()
			}
			before, err := os.Stat(filepath.Join(otherDir, "unrelated.heartbeat"))
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(120 * time.Millisecond)
			for role, size := range sizes {
				st, err := os.Stat(filepath.Join(dir, role+".heartbeat"))
				if err != nil {
					t.Fatal(err)
				}
				if st.Size() != size {
					t.Fatalf("%s process survived cleanup", role)
				}
			}
			after, err := os.Stat(filepath.Join(otherDir, "unrelated.heartbeat"))
			if err != nil {
				t.Fatal(err)
			}
			if after.Size() <= before.Size() {
				t.Fatal("cleanup killed an unrelated process")
			}
		})
	}
}

func TestProfileCleanupErrorsRemainObservableAndIdempotent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager("", 1)
	// A non-directory parent can report PATH_NOT_FOUND on Windows, which
	// RemoveAll correctly treats as already removed. NUL is invalid on all
	// supported platforms and reliably exercises a real cleanup failure.
	s := &Session{Dir: file + "\x00profile", owner: m}
	m.active[s] = struct{}{}
	first := s.Close()
	if first == nil {
		t.Fatal("profile removal failure was hidden")
	}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Close(); err == nil || err.Error() != first.Error() {
				t.Error("repeated Close lost cleanup error")
			}
		}()
	}
	wg.Wait()
	if err := m.Close(); err == nil || err.Error() != first.Error() {
		t.Fatalf("manager did not retain cleanup error: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(fmt.Errorf("cleanup changed parent file: %w", err))
	}
}
