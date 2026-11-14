package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errBuildBusy = errors.New("another build or frontend check is using this workspace; retry after it exits")

// Keep the inode in place: deleting a locked file would let another process
// acquire a different inode. OS locks are released even after forced exit.
func acquireBuildLock(root string) (func(), error) {
	path := filepath.Join(root, ".build-web.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("frontend build lock must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open frontend build lock: %w", err)
	}
	opened, openErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if openErr != nil || pathErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("frontend build lock changed while opening")
	}
	if err := lockBuildFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}
