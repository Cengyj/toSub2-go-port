//go:build !windows

package credentials

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"tosub2-go-port/internal/privatefile"
)

func openFileKeyDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || info.Uid != uint32(os.Geteuid()) || info.Mode&0077 != 0 {
		_ = file.Close()
		return nil, errors.New("credential directory must be private and owned by the service user")
	}
	return file, nil
}

// Open relative to a verified directory and reject links before reading. The
// nonblocking open prevents a malicious FIFO/device from hanging startup.
func readFileKeyPrivate(path string, limit int64) ([]byte, error) {
	dir, err := openFileKeyDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Geteuid()) || info.Nlink != 1 || (info.Mode&0777 != 0600 && info.Mode&0777 != 0400) {
		return nil, errors.New("credential file must be private, regular and owned by the service user")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		clear(raw)
		return nil, errors.New("credential file exceeds its supported size or could not be read")
	}
	return raw, nil
}

func withFileKeyRootLock(ctx context.Context, root string, action func() error) error {
	if err := privatefile.EnsureDir(root); err != nil {
		return fmt.Errorf("credential key directory unavailable: %w", err)
	}
	dir, err := openFileKeyDirectory(root)
	if err != nil {
		return fmt.Errorf("credential key directory unavailable: %w", err)
	}
	defer dir.Close()
	fd, err := openFileKeyLock(int(dir.Fd()))
	if err != nil {
		return fmt.Errorf("credential key initialization lock unavailable: %w", err)
	}
	file := os.NewFile(uintptr(fd), fileKeyLock)
	defer file.Close()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Geteuid()) || info.Mode&0777 != 0600 || info.Nlink != 1 {
		return errors.New("credential key initialization lock is not private")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return errors.New("credential key initialization could not acquire lock")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	return action()
}

func openFileKeyLock(dirfd int) (int, error) {
	const flags = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	for attempt := 0; attempt < 16; attempt++ {
		fd, err := unix.Openat(dirfd, fileKeyLock, flags, 0)
		if !errors.Is(err, unix.ENOENT) {
			return fd, err
		}
		fd, err = unix.Openat(dirfd, fileKeyLock, flags|unix.O_CREAT|unix.O_EXCL, 0600)
		if !errors.Is(err, unix.EEXIST) {
			return fd, err
		}
	}
	return -1, errors.New("credential lock changed repeatedly during initialization")
}

func createFileKey(path string) error {
	dirPath := filepath.Dir(path)
	if err := privatefile.EnsureDir(dirPath); err != nil {
		return fmt.Errorf("credential master key directory unavailable: %w", err)
	}
	// EnsureDir may create several parents. Flush their directory entries
	// before binding any credential data to the new key.
	for current := dirPath; ; current = filepath.Dir(current) {
		if err := privatefile.SyncDirectory(current); err != nil {
			return errors.New("credential master key directory could not be synchronized")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	dir, err := openFileKeyDirectory(dirPath)
	if err != nil {
		return errors.New("credential master key directory is not private")
	}
	defer dir.Close()
	file, err := privatefile.CreateTemp(dirPath, ".master-key-*.tmp")
	if err != nil {
		return errors.New("credential master key temporary file unavailable")
	}
	name := file.Name()
	defer os.Remove(name)
	key := make([]byte, 32)
	defer clear(key)
	if _, err = rand.Read(key); err == nil {
		_, err = file.Write(key)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("credential master key could not be durably written")
	}
	// Linking a complete, flushed file publishes it atomically without ever
	// overwriting an existing key, even across unrelated initializer processes.
	if err = unix.Linkat(int(dir.Fd()), filepath.Base(name), int(dir.Fd()), filepath.Base(path), 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return errors.New("credential master key could not be published")
	}
	if err = unix.Unlinkat(int(dir.Fd()), filepath.Base(name), 0); err != nil {
		return fmt.Errorf("%w: credential master key temporary link could not be removed", privatefile.ErrDurabilityUncertain)
	}
	if err = dir.Sync(); err != nil {
		return fmt.Errorf("%w: credential master key directory could not be synchronized", privatefile.ErrDurabilityUncertain)
	}
	return nil
}
