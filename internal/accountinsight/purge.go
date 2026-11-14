package accountinsight

import (
	"context"
	"errors"
	"os"
	"runtime"
)

var ErrCacheBusy = errors.New("账号仍在活动列表或查询尚未结束，缓存暂不可彻底清理")

// PurgeAccountIDs removes only the named local observations. The owner must
// first durably mark these accounts as purging and prevent their restoration.
// RetainedIDs intentionally does not protect this explicit purge. Active
// accounts and outstanding query ownership still block it, without acquiring
// any grant or contacting an upstream service.
func (s *Service) PurgeAccountIDs(ctx context.Context, ids []string) error {
	return s.purgeAccountIDs(ctx, ids, os.Remove)
}

func (s *Service) purgeAccountIDs(ctx context.Context, ids []string, remove func(string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrCacheCleanup, err)
	}
	targets := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			return ErrCacheCleanup
		}
		targets[id] = true
	}
	if len(targets) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrCacheCleanup, err)
	}
	// Reject a mixed selection before deleting any local data. Running flights
	// remain registered after cancellation until their transport lease exits.
	active := accountMap(s.provider.Accounts())
	s.mu.Lock()
	for id := range targets {
		if _, exists := active[id]; exists || s.flights[id] != nil {
			s.mu.Unlock()
			return ErrCacheBusy
		}
	}
	s.mu.Unlock()
	root, err := os.Lstat(s.disk.Root)
	if err != nil || !root.IsDir() {
		return ErrCacheCleanup
	}
	for id := range targets {
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrCacheCleanup, err)
		}
		path := s.disk.Path(snapshotFile(id))
		before, statErr := os.Lstat(path)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) || statErr == nil && !before.Mode().IsRegular() {
			return ErrCacheCleanup
		}
		latestRoot, rootErr := os.Lstat(s.disk.Root)
		if rootErr != nil || !latestRoot.IsDir() || !os.SameFile(root, latestRoot) {
			return ErrCacheCleanup
		}
		active = accountMap(s.provider.Accounts())
		s.mu.Lock()
		if _, exists := active[id]; exists || s.flights[id] != nil {
			s.mu.Unlock()
			return ErrCacheBusy
		}
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return errors.Join(ErrCacheCleanup, err)
		}
		if statErr == nil {
			latest, latestErr := os.Lstat(path)
			if latestErr != nil || !latest.Mode().IsRegular() || !os.SameFile(before, latest) {
				s.mu.Unlock()
				return ErrCacheCleanup
			}
			if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.mu.Unlock()
				return ErrCacheCleanup
			}
		}
		if _, exists := s.snapshots[id]; exists {
			delete(s.snapshots, id)
			s.version++
		}
		s.mu.Unlock()
	}
	// Sync even when all files were already absent: a previous attempt may
	// have removed them but failed to confirm directory durability.
	if runtime.GOOS != "windows" {
		dir, openErr := os.Open(s.disk.Root)
		if openErr != nil {
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
