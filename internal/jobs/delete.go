package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"tosub2-go-port/internal/credentials"
)

// DeleteAccounts preserves the console's email-scoped deletion semantics. It
// cancels runners first, waits for their browser/transport cleanup, then removes
// every matching legacy record, private output directory and saved credential.
// A blocked runner leaves a canceled record recoverable instead of deleting
// files underneath a live process that could recreate them.
func (m *Manager) DeleteAccounts(ctx context.Context, ids []string) (int, error) {
	m.mu.Lock()
	emails := map[string]string{}
	for _, id := range ids {
		j, exists := m.jobs[id]
		if !exists {
			m.mu.Unlock()
			return 0, osErrNotFound(id)
		}
		key := emailKey(j.Email)
		if m.deleting[key] || m.importing[key] {
			m.mu.Unlock()
			return 0, errors.New("account is already being deleted or restored")
		}
		emails[key] = j.Email
	}
	for key := range emails {
		m.deleting[key] = true
	}
	targets := []string{}
	for id, job := range m.jobs {
		if _, selected := emails[emailKey(job.Email)]; !selected {
			continue
		}
		targets = append(targets, id)
		delete(m.restart, id)
		m.removePendingLocked(id)
		if cancel, active := m.active[id]; active {
			cancel()
		} else if job.Status == StatusQueued {
			job.Status = StatusCanceled
			now := time.Now().UTC()
			job.UpdatedAt, job.FinishedAt = now, &now
		}
	}
	m.persistLocked()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		for key := range emails {
			delete(m.deleting, key)
		}
		m.mu.Unlock()
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		m.mu.Lock()
		active := false
		for _, id := range targets {
			if _, running := m.active[id]; running {
				active = true
			}
		}
		m.mu.Unlock()
		if !active {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, email := range emails {
		if m.vault != nil {
			if err := credentials.DeleteContext(ctx, m.vault, email); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, errors.New("secure credential deletion failed")
			}
		}
	}
	for _, id := range targets {
		if m.store != nil {
			if err := os.RemoveAll(filepath.Join(m.store.Root, id)); err != nil {
				return 0, errors.New("private job directory deletion failed")
			}
		}
	}
	for _, id := range targets {
		delete(m.jobs, id)
	}
	m.persistLocked()
	if m.lastPersistError != nil {
		return len(emails), errors.New("job deletion metadata could not be saved")
	}
	return len(emails), nil
}

// QueuePosition reports the scheduler's FIFO position, independent of the
// reverse chronological order used by the web console.
func (m *Manager) QueuePosition(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, job := range m.pending {
		if job.ID == id && job.Status == StatusQueued {
			return i + 1
		}
	}
	return 0
}
