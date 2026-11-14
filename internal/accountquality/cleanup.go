package accountquality

import (
	"context"
	"errors"
	"os"
	"runtime"
)

// PurgeAccountIDs is called only after the owner has durably marked the accounts
// purging. RetainedIDs protects loading/history, but never blocks explicit purge.
func (s *Service) PurgeAccountIDs(ctx context.Context, ids []string) error {
	return s.purgeAccountIDs(ctx, ids, os.Remove)
}
func (s *Service) purgeAccountIDs(ctx context.Context, ids []string, remove func(string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	targets := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			return ErrCacheCleanup
		}
		targets[id] = true
	}
	if len(targets) == 0 {
		return nil
	}
	if err := s.lockWrite(ctx); err != nil {
		return err
	}
	defer s.unlockWrite()
	busy := func() bool {
		active := accountMap(s.provider.Accounts())
		s.mu.Lock()
		defer s.mu.Unlock()
		for id := range targets {
			if _, ok := active[id]; ok {
				return true
			}
			for _, f := range s.flights {
				if f.id == id {
					return true
				}
			}
		}
		return false
	}
	if busy() {
		return ErrCacheBusy
	}
	root, err := os.Lstat(s.disk.Root)
	if err != nil || !root.IsDir() {
		return ErrCacheCleanup
	}
	for id := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if busy() {
			return ErrCacheBusy
		}
		path := s.disk.Path(snapshotFile(id))
		before, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !before.Mode().IsRegular() {
			return ErrCacheCleanup
		}
		latest, rootErr := os.Lstat(s.disk.Root)
		if rootErr != nil || !latest.IsDir() || !os.SameFile(root, latest) {
			return ErrCacheCleanup
		}
		if err == nil {
			if err = remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return ErrCacheCleanup
			}
		}
		s.mu.Lock()
		delete(s.rows, id)
		s.version++
		s.mu.Unlock()
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(s.disk.Root)
		if err != nil {
			return ErrCacheCleanup
		}
		opened, statErr := dir.Stat()
		if statErr != nil || !opened.IsDir() || !os.SameFile(root, opened) {
			_ = dir.Close()
			return ErrCacheCleanup
		}
		syncErr, closeErr := dir.Sync(), dir.Close()
		if syncErr != nil || closeErr != nil {
			return ErrCacheCleanup
		}
	}
	return nil
}
