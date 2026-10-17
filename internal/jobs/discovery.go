package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SyncOutputs mirrors the legacy console's periodic discovery of CLI output
// directories. Existing live records are authoritative and are never reloaded.
// Disk and credential reads run without holding the scheduler's global lock.
func (m *Manager) SyncOutputs(ctx context.Context, force bool) error {
	if m.store == nil {
		return nil
	}
	select {
	case m.importGate <- struct{}{}:
		defer func() { <-m.importGate }()
	case <-ctx.Done():
		return ctx.Err()
	case <-m.rootCtx.Done():
		return m.rootCtx.Err()
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.rootCtx, cancel)
	defer stop()
	defer cancel()
	m.mu.Lock()
	if m.closing || (!force && time.Since(m.lastOutputSyncAt) < 2*time.Second) {
		m.mu.Unlock()
		return nil
	}
	m.lastOutputSyncAt = time.Now()
	vault := m.vault
	m.mu.Unlock()
	entries, err := os.ReadDir(m.store.Root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !safeID(entry.Name()) {
			continue
		}
		id := entry.Name()
		m.mu.Lock()
		_, exists := m.jobs[id]
		m.mu.Unlock()
		if exists {
			continue
		}
		raw, _ := readRegularJSON(filepath.Join(m.store.Root, id, "job-meta.json"))
		job, valid := decodeLegacyJob(raw, id)
		if !valid {
			email, output, checkpoint := recoveryArtifacts(m.store.Root, id)
			if email == "" || (!output && !checkpoint) {
				continue
			}
			job = &Job{ID: id, Email: email}
		}
		key := emailKey(job.Email)
		m.mu.Lock()
		_, exists = m.jobs[id]
		if exists || m.closing || m.deleting[key] || m.importing[key] || m.emailUse[key] != "" {
			m.mu.Unlock()
			continue
		}
		// Reserve this account so recovery cannot overwrite credentials while a
		// login, deletion, or another explicit account operation is in progress.
		m.importing[key] = true
		m.mu.Unlock()
		importCtx, importCancel := context.WithTimeout(ctx, 3*time.Second)
		err := m.prepareRestoredJob(importCtx, id, job, vault)
		importCancel()
		m.mu.Lock()
		delete(m.importing, key)
		_, exists = m.jobs[id]
		info, diskErr := os.Lstat(filepath.Join(m.store.Root, id))
		if err == nil && diskErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && !exists && !m.closing && ctx.Err() == nil {
			m.jobs[id] = job
			if job.Status == StatusQueued {
				m.pending = append(m.pending, job)
				sort.SliceStable(m.pending, func(i, j int) bool {
					a, b := m.pending[i], m.pending[j]
					if a.QueuedAt.Equal(b.QueuedAt) {
						if a.CreatedAt.Equal(b.CreatedAt) {
							return a.ID < b.ID
						}
						return a.CreatedAt.Before(b.CreatedAt)
					}
					return a.QueuedAt.Before(b.QueuedAt)
				})
			}
			m.persistLocked()
			if m.lastPersistError != nil {
				delete(m.jobs, id)
				m.removePendingLocked(id)
				err = errors.New("discovered job metadata could not be saved")
			} else {
				err = m.cleanupRestoredResults(job)
			}
		}
		m.mu.Unlock()
		select {
		case m.wake <- struct{}{}:
		default:
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
