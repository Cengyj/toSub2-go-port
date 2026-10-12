//go:build windows

package legacyruntime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processTree struct {
	mu       sync.Mutex
	job      windows.Handle
	attached bool
	stopped  bool
	err      error
}

func (t *processTree) finalError(_ *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func prepareProcessTree(cmd *exec.Cmd) (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create legacy runtime process job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure legacy runtime process job: %w", err)
	}
	tree := &processTree{job: job}
	// Do not allow the new worker to execute (and spawn unowned children)
	// until it has joined this job. Go closes the primary thread handle in
	// Start, so attach resumes it through a thread snapshot after assignment.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	cmd.Cancel = func() error { return tree.terminate(cmd.Process) }
	cmd.WaitDelay = 3 * time.Second
	return tree, nil
}

func (t *processTree) attach(process *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return ErrClosed
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		return fmt.Errorf("open legacy runtime process for job: %w", err)
	}
	defer windows.CloseHandle(handle)
	if err = windows.AssignProcessToJobObject(t.job, handle); err != nil {
		return fmt.Errorf("assign legacy runtime process job: %w", err)
	}
	t.attached = true
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot suspended legacy runtime thread: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(process.Pid) {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fmt.Errorf("open suspended legacy runtime thread: %w", openErr)
		}
		_, resumeErr := windows.ResumeThread(thread)
		closeErr := windows.CloseHandle(thread)
		if err := errors.Join(resumeErr, closeErr); err != nil {
			return fmt.Errorf("resume assigned legacy runtime process: %w", err)
		}
		return nil
	}
	if err == nil || errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		err = errors.New("suspended primary thread not found")
	}
	return fmt.Errorf("find suspended legacy runtime thread: %w", err)
}

func (t *processTree) terminate(process *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return t.err
	}
	t.stopped = true
	var failures []error
	if t.job != 0 {
		if err := windows.TerminateJobObject(t.job, 1); err != nil {
			failures = append(failures, fmt.Errorf("terminate legacy runtime process job: %w", err))
		}
		// TerminateJobObject is asynchronous. Wait for the whole job, including
		// renderers and crash handlers, before removing the private profile.
		if t.attached {
			if err := waitForEmptyJob(t.job, 3*time.Second); err != nil {
				failures = append(failures, err)
			}
		}
		if err := windows.CloseHandle(t.job); err != nil {
			failures = append(failures, fmt.Errorf("close legacy runtime process job: %w", err))
		}
		t.job = 0
	}
	if process != nil && !t.attached {
		if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			failures = append(failures, err)
		}
	}
	t.err = errors.Join(failures...)
	return t.err
}

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, whose public ABI contains four
// LARGE_INTEGER values, four DWORD counters, and the termination counter.
type jobAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func waitForEmptyJob(job windows.Handle, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info := jobAccountingInformation{}
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
		if err != nil {
			return fmt.Errorf("inspect legacy runtime process job: %w", err)
		}
		if info.ActiveProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("legacy runtime process job did not stop before cleanup deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
