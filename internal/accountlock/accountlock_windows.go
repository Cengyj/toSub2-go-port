//go:build windows

package accountlock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockFileMode(file *os.File, shared bool) error {
	var overlap windows.Overlapped
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &overlap)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrBusy
	}
	return err
}
