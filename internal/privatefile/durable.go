package privatefile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrDurabilityUncertain means the requested replacement or removal is already
// visible, but its directory entry could not be confirmed on durable storage.
// Callers must retain their pending state and must not retry a remote action.
var ErrDurabilityUncertain = errors.New("private file changed but durable storage could not be confirmed")

type durableOps struct {
	replace func(string, string) error
	remove  func(string) error
	syncDir func(string) error
}

func platformDurableOps() durableOps {
	return durableOps{replace: replaceDurable, remove: os.Remove, syncDir: syncDirectory}
}

// SyncDirectory confirms pending directory-entry changes on durable storage.
// Code that already changed an entry must wrap failures with
// ErrDurabilityUncertain; Write and Remove do that automatically.
func SyncDirectory(path string) error { return syncDirectory(path) }

// Write atomically replaces path with a private, flushed file and then flushes
// its containing directory. Existing export-directory permissions are kept.
// Cancellation before replacement preserves the previous file. Once replaced,
// directory synchronization completes even if the context has been canceled.
func Write(ctx context.Context, path string, data []byte) error {
	return writeDurable(ctx, path, data, platformDurableOps())
}

func writeDurable(ctx context.Context, path string, data []byte, ops durableOps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDurableParent(dir, ops.syncDir); err != nil {
		return err
	}
	file, err := CreateTemp(dir, ".private-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	n, err := file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write private temporary file: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = ops.replace(name, path); err != nil {
		return fmt.Errorf("replace private file: %w", err)
	}
	if err = ops.syncDir(dir); err != nil {
		return fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	return nil
}

// Remove durably removes a file. An absent file is idempotent, but its existing
// parent is still synchronized so retrying an uncertain removal can confirm it.
func Remove(ctx context.Context, path string) error {
	return removeDurable(ctx, path, platformDurableOps())
}

func removeDurable(ctx context.Context, path string, ops durableOps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := ops.remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove private file: %w", err)
	}
	dir := filepath.Dir(path)
	if err != nil {
		if _, parentErr := os.Stat(dir); errors.Is(parentErr, os.ErrNotExist) {
			return nil
		} else if parentErr != nil {
			return parentErr
		}
	}
	if err = ops.syncDir(dir); err != nil {
		return fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	return nil
}

// Creating a parent also changes its parent's directory entry. Confirm the
// ancestor chain before publishing a file below it. Existing directories must
// be included: they may come from an earlier attempt whose parent flush failed.
func ensureDurableParent(dir string, syncDir func(string) error) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := EnsureParentDir(absolute); err != nil {
		return err
	}
	var ancestors []string
	for current := filepath.Dir(absolute); ; current = filepath.Dir(current) {
		ancestors = append(ancestors, current)
		if filepath.Dir(current) == current {
			break
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		if err := syncDir(ancestors[i]); err != nil {
			return fmt.Errorf("synchronize private directory ancestry: %w", err)
		}
	}
	return nil
}
