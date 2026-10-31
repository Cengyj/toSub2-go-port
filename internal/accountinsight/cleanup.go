package accountinsight

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"

	"tosub2-go-port/internal/privatefile"
)

var ErrCacheCleanup = errors.New("账号查询缓存未完全清理，请检查数据目录后重试")

// PruneDeleted removes only verifiable snapshots for IDs that no longer exist.
// It never touches credentials, account output, settings or unrecognized files,
// and never releases a running query transport lease.
func (s *Service) PruneDeleted(ctx context.Context) error {
	return s.pruneDeleted(ctx, os.Remove)
}

// The removal seam keeps filesystem-failure tests independent of OS privileges.
func (s *Service) pruneDeleted(ctx context.Context, remove func(string) error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrCacheCleanup, err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		warnings := strings.Split(s.lastError, "；")
		kept := warnings[:0]
		for _, warning := range warnings {
			if warning != "" && warning != ErrCacheCleanup.Error() {
				kept = append(kept, warning)
			}
		}
		if err != nil {
			kept = append(kept, ErrCacheCleanup.Error())
		}
		s.lastError = strings.Join(kept, "；")
	}()
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrCacheCleanup, err)
	}
	current := accountMap(s.provider.Accounts())
	currentFiles := make(map[string]bool, len(current))
	for id := range current {
		currentFiles[snapshotFile(id)] = true
	}
	s.mu.Lock()
	changed := false
	for id, t := range s.flights {
		if _, exists := current[id]; exists {
			continue
		}
		if t.cancel != nil {
			t.cancel()
		}
		delete(s.flights, id)
		changed = true
	}
	pending := s.pending[:0]
	for _, t := range s.pending {
		if _, exists := current[t.id]; exists {
			pending = append(pending, t)
		} else {
			changed = true
		}
	}
	clear(s.pending[len(pending):])
	s.pending = pending
	for id := range s.snapshots {
		if _, exists := current[id]; !exists {
			delete(s.snapshots, id)
			changed = true
		}
	}
	if changed {
		s.version++
	}
	s.mu.Unlock()
	if changed {
		s.notify()
	}
	root, rootErr := os.Lstat(s.disk.Root)
	if rootErr != nil || !root.IsDir() {
		return ErrCacheCleanup
	}
	entries, readErr := os.ReadDir(s.disk.Root)
	if readErr != nil {
		return ErrCacheCleanup
	}
	incomplete, removed := false, false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrCacheCleanup, err)
		}
		name := entry.Name()
		if currentFiles[name] || len(name) != 69 || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := s.disk.Path(name)
		before, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			incomplete = true
			continue
		}
		if !before.Mode().IsRegular() {
			continue
		}
		var row storedSnapshot
		if readErr := s.read(name, &row); readErr != nil {
			// Unknown JSON is not ours to delete. An I/O or permission failure
			// cannot be described as a successful privacy cleanup either.
			var pathErr *os.PathError
			if errors.Is(readErr, privatefile.ErrPermissions) || errors.As(readErr, &pathErr) && !errors.Is(readErr, os.ErrNotExist) {
				incomplete = true
			}
			continue
		}
		if row.SchemaVersion != 1 || row.Snapshot.ID == "" || snapshotFile(row.Snapshot.ID) != name {
			continue
		}
		if _, exists := accountMap(s.provider.Accounts())[row.Snapshot.ID]; exists {
			continue
		}
		latestRoot, rootErr := os.Lstat(s.disk.Root)
		latest, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if rootErr != nil || !latestRoot.IsDir() || !os.SameFile(root, latestRoot) || statErr != nil || !latest.Mode().IsRegular() || !os.SameFile(before, latest) {
			incomplete = true
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrCacheCleanup, err)
		}
		if removeErr := remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			incomplete = true
		} else {
			removed = true
		}
	}
	if removed && runtime.GOOS != "windows" {
		dir, openErr := os.Open(s.disk.Root)
		if openErr != nil {
			incomplete = true
		} else {
			syncErr := dir.Sync()
			closeErr := dir.Close()
			if syncErr != nil || closeErr != nil {
				incomplete = true
			}
		}
	}
	if incomplete {
		return ErrCacheCleanup
	}
	return nil
}
