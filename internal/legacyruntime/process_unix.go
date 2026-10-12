//go:build !windows

package legacyruntime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Each script runtime owns a new process group. Kill only that group; unrelated
// processes on the machine do not belong to this login session.
type processTree struct {
	mu      sync.Mutex
	stopped bool
	err     error
}

func prepareProcessTree(cmd *exec.Cmd) (*processTree, error) {
	tree := &processTree{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return tree.terminate(cmd.Process) }
	cmd.WaitDelay = 3 * time.Second
	return tree, nil
}

func (t *processTree) attach(_ *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return ErrClosed
	}
	return nil
}

func (t *processTree) terminate(process *os.Process) error {
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
		t.err = fmt.Errorf("terminate legacy runtime process group: %w", err)
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

func (t *processTree) finalError(process *os.Process) error {
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
			return errors.New("legacy runtime process group still exists after cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
