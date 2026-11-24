//go:build !windows

package console

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// This test-only policy follows the script worker ownership boundary without
// exporting production internals or adding a browser runtime dependency.
// Each browser fixture owns a new process group. Kill only that group; unrelated
// processes on the machine do not belong to this browser test.
type frontendBrowserProcessTree struct {
	mu      sync.Mutex
	stopped bool
	err     error
}

func prepareFrontendBrowserProcessTree(cmd *exec.Cmd) (*frontendBrowserProcessTree, error) {
	tree := &frontendBrowserProcessTree{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return tree.terminate(cmd.Process) }
	cmd.WaitDelay = frontendBrowserDrainDelay
	return tree, nil
}

func (t *frontendBrowserProcessTree) attach(_ *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return errors.New("browser fixture process ownership is closed")
	}
	return nil
}

func (t *frontendBrowserProcessTree) terminate(process *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return t.err
	}
	t.stopped = true
	if process == nil {
		return nil
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.err = fmt.Errorf("terminate browser fixture process group: %w", err)
		if fallbackErr := process.Kill(); fallbackErr != nil && !errors.Is(fallbackErr, os.ErrProcessDone) {
			t.err = errors.Join(t.err, fallbackErr)
		}
		// macOS can report EPERM while sandboxed descendants are already
		// tearing down after their parent exits. Only an observed ESRCH proves
		// that this exact group is gone; otherwise preserve the kill failure.
		deadline := time.Now().Add(time.Second)
		for {
			if errors.Is(syscall.Kill(-process.Pid, 0), syscall.ESRCH) {
				t.err = nil
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return t.err
}

func (t *frontendBrowserProcessTree) finalError(process *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if process == nil {
		return t.err
	}
	deadline := time.Now().Add(time.Second)
	for {
		if errors.Is(syscall.Kill(-process.Pid, 0), syscall.ESRCH) {
			t.err = nil
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("browser fixture process group still exists after cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
