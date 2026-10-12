//go:build !windows

package legacyruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestProcessTreeCleanupStopsGoWorkerDescendants(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"request-cancel", "owner-cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			c, tmp := bridgeTestClient(t, "process-tree", "")
			pidFile := filepath.Join(t.TempDir(), "pids.json")
			owner, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			operation, cancelOperation := context.WithCancel(owner)
			defer cancelOperation()
			r, err := c.startRuntime(operation, "parent", map[string]string{"pidFile": pidFile}, make(chan runtimeEvent, 2))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := r.close(); err != nil {
					t.Error(err)
				}
			})
			deadline := time.Now().Add(3 * time.Second)
			var pids struct{ Parent, Child, Grandchild int }
			for {
				data, err := os.ReadFile(pidFile)
				if err == nil && json.Unmarshal(data, &pids) == nil && pids.Grandchild > 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Go process tree fixture did not initialize")
				}
				time.Sleep(10 * time.Millisecond)
			}
			owned := []int{pids.Parent, pids.Child, pids.Grandchild}
			for _, pid := range owned {
				if pid <= 1 || syscall.Kill(pid, 0) != nil {
					t.Fatalf("fixture process %d was not alive", pid)
				}
			}
			other := exec.Command(executable, "--process-tree-fixture", "grandchild", "", "")
			if err := other.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
			switch mode {
			case "request-cancel":
				cancelOperation()
			case "owner-cancel":
				cancelOwner()
			case "close":
				if err := r.close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.close(); err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(3 * time.Second)
			for _, pid := range owned {
				for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
					if time.Now().After(deadline) {
						t.Fatalf("owned process %d survived %s", pid, mode)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if err := syscall.Kill(other.Process.Pid, 0); err != nil {
				t.Fatal("cleanup killed an unrelated Go process")
			}
			assertRuntimeInputsRemoved(t, tmp)
		})
	}
}
