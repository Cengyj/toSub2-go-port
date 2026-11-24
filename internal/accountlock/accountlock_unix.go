//go:build !windows

package accountlock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockFileMode(file *os.File, shared bool) error {
	mode := unix.LOCK_EX
	if shared {
		mode = unix.LOCK_SH
	}
	err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrBusy
	}
	return err
}
