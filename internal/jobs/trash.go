package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

const (
	TrashStateTrashed = "trashed"
	TrashStatePurging = "purging"
)

var (
	ErrAccountInTrash         = errors.New("账号已在回收站，请先恢复或永久删除后再导入")
	ErrTrashPersistence       = errors.New("回收站变更保存失败，请检查数据目录后重试")
	ErrTrashUncertain         = errors.New("回收站变更已写入，但持久性尚未确认；请修复存储后重新确认，勿立即重启")
	ErrTrashPurging           = errors.New("账号已开始永久删除，不能恢复；请重试完成清理")
	ErrTrashNotFound          = errors.New("回收记录已变化或已处理，请刷新后核对")
	ErrTrashBusy              = errors.New("账号正在执行其他操作，暂时无法处理，请稍后重试")
	ErrTrashConflict          = errors.New("回收记录与其他账号数据存在冲突，已停止清理，请核对账号记录")
	ErrTrashInspection        = errors.New("账号历史文件检查未完成，请检查本地数据目录后重试")
	ErrTrashLocalCleanup      = errors.New("关联账号数据清理未完成，待清理记录已保留")
	ErrTrashDirectoryCleanup  = errors.New("账号本地文件删除未完成，请检查数据目录权限后重试")
	ErrTrashCredentialCleanup = errors.New("安全凭据清理未完成，请检查系统凭据服务后重试")
	ErrTrashInsightCleanup    = errors.New("账号用量缓存清理未完成，请检查用量缓存存储后重试")
	ErrTrashQualityCleanup    = errors.New("模型检测缓存清理未完成，请检查模型检测缓存存储后重试")
	ErrTrashSMSCleanup        = errors.New("短信账号数据清理未完成，请检查本地短信数据存储后重试")
)

// TrashCleanupFailure preserves machine-readable causes while keeping private
// paths, account identifiers and credential-service output out of public errors.
// Callers may select only a known, fixed stage; arbitrary stage text is ignored.
func TrashCleanupFailure(stage error, cause error) error {
	switch stage {
	case ErrTrashNotFound, ErrTrashBusy, ErrTrashConflict, ErrTrashInspection,
		ErrTrashLocalCleanup, ErrTrashDirectoryCleanup, ErrTrashCredentialCleanup,
		ErrTrashInsightCleanup, ErrTrashQualityCleanup, ErrTrashSMSCleanup:
	default:
		stage = ErrTrashLocalCleanup
	}
	return &trashCleanupFailure{stage: stage, cause: cause}
}

type trashCleanupFailure struct {
	stage error
	cause error
}

func (e *trashCleanupFailure) Error() string { return e.stage.Error() }
func (e *trashCleanupFailure) Unwrap() []error {
	if e.cause == nil {
		return []error{e.stage}
	}
	return []error{e.stage, e.cause}
}

func trashHookFailure(err error) error {
	for _, stage := range []error{ErrTrashBusy, ErrTrashConflict, ErrTrashInsightCleanup, ErrTrashQualityCleanup, ErrTrashSMSCleanup} {
		if errors.Is(err, stage) {
			return TrashCleanupFailure(stage, err)
		}
	}
	return TrashCleanupFailure(ErrTrashLocalCleanup, err)
}

// TrashEntry retains an entire normalized-email history independently of the
// active scheduler. Private artifacts and credentials remain in their existing
// locations until an explicitly committed permanent purge.
type TrashEntry struct {
	ID             string     `json:"id"`
	Email          string     `json:"email"`
	DeletedAt      time.Time  `json:"deletedAt"`
	PurgeStartedAt *time.Time `json:"purgeStartedAt,omitempty"`
	State          string     `json:"state"`
	LastError      string     `json:"lastError,omitempty"`
	Jobs           []*Job     `json:"jobs"`
}

// SetTrashPauseHook suspends local external ownership with the email gate held,
// before canceling any worker, without mu. Failure must leave data retained.
func (m *Manager) SetTrashPauseHook(h func(context.Context, []*Job) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trashPauseHook = h
}

// SetTrashPurgeHook installs idempotent local cleanup for data owned outside
// jobs. It runs only after a durable purging tombstone and without mu held.
func (m *Manager) SetTrashPurgeHook(h func(context.Context, *TrashEntry) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trashPurgeHook = h
}

func (m *Manager) ListTrash() []*TrashEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*TrashEntry, 0, len(m.trash))
	for _, entry := range m.trash {
		out = append(out, publicTrashEntry(entry))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeletedAt.Equal(out[j].DeletedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].DeletedAt.After(out[j].DeletedAt)
	})
	return out
}

func (m *Manager) GetTrash(id string) (*TrashEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.trash[id]
	if !ok {
		return nil, false
	}
	return publicTrashEntry(entry), true
}

func publicTrashEntry(entry *TrashEntry) *TrashEntry {
	copy := *entry
	if entry.PurgeStartedAt != nil {
		at := *entry.PurgeStartedAt
		copy.PurgeStartedAt = &at
	}
	copy.Jobs = make([]*Job, 0, len(entry.Jobs))
	for _, job := range entry.Jobs {
		copy.Jobs = append(copy.Jobs, SanitizeJobForPublic(job))
	}
	sort.Slice(copy.Jobs, func(i, j int) bool {
		if copy.Jobs[i].CreatedAt.Equal(copy.Jobs[j].CreatedAt) {
			return copy.Jobs[i].ID < copy.Jobs[j].ID
		}
		return copy.Jobs[i].CreatedAt.Before(copy.Jobs[j].CreatedAt)
	})
	return &copy
}

// RetainedAccountIDs includes trash so restart cannot prune caches while the
// account remains recoverable or permanent cleanup is incomplete.
func (m *Manager) RetainedAccountIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := make(map[string]struct{}, len(m.jobs))
	for id := range m.jobs {
		set[id] = struct{}{}
	}
	for _, entry := range m.trash {
		for _, job := range entry.Jobs {
			set[job.ID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) trashContainsLocked(id, email string) bool {
	key := emailKey(email)
	for _, entry := range m.trash {
		if key != "" && emailKey(entry.Email) == key {
			return true
		}
		if id != "" {
			for _, job := range entry.Jobs {
				if job.ID == id {
					return true
				}
			}
		}
	}
	return false
}

func (m *Manager) trashMutationAllowedLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closing || m.rootCtx.Err() != nil {
		return errors.New("job manager is shutting down")
	}
	return nil
}

func (m *Manager) releaseTrashGates(emails map[string]bool) {
	m.mu.Lock()
	for key := range emails {
		delete(m.deleting, key)
	}
	m.mu.Unlock()
	m.wakeScheduler()
}

// TrashAccounts stops and retains every record for each selected email. No
// private files or credentials are deleted. Active jobs and trash commit in one
// atomic metadata replacement before success becomes visible.
func (m *Manager) TrashAccounts(ctx context.Context, ids []string) (int, error) {
	m.mu.Lock()
	if err := m.trashMutationAllowedLocked(ctx); err != nil {
		m.mu.Unlock()
		return 0, err
	}
	emails := map[string]bool{}
	for _, id := range ids {
		job, ok := m.jobs[id]
		if !ok {
			m.mu.Unlock()
			return 0, osErrNotFound(id)
		}
		key := emailKey(job.Email)
		if key == "" || m.deleting[key] || m.importing[key] {
			m.mu.Unlock()
			return 0, errors.New("account is already being changed")
		}
		if m.trashContainsLocked(id, key) {
			m.mu.Unlock()
			return 0, ErrAccountInTrash
		}
		emails[key] = true
	}
	if len(emails) == 0 {
		m.mu.Unlock()
		return 0, nil
	}
	targets := make([]*Job, 0)
	for _, job := range m.jobs {
		if emails[emailKey(job.Email)] {
			targets = append(targets, job)
		}
	}
	for key := range emails {
		m.deleting[key] = true
	}
	hook := m.trashPauseHook
	hookJobs := make([]*Job, 0, len(targets))
	for _, job := range targets {
		hookJobs = append(hookJobs, SanitizeJobForPublic(job))
	}
	m.mu.Unlock()
	defer m.releaseTrashGates(emails)
	if hook != nil {
		if err := hook(ctx, hookJobs); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, fmt.Errorf("account resources could not be safely paused: %s", safeError(err))
		}
	}
	m.mu.Lock()
	if err := m.trashMutationAllowedLocked(ctx); err != nil {
		m.mu.Unlock()
		return 0, err
	}
	for key := range emails {
		m.cancelAccountQueryLocked(key)
	}
	for _, job := range targets {
		delete(m.restart, job.ID)
		delete(m.inputPreparing, job.ID)
		m.removePendingLocked(job.ID)
		if cancel := m.active[job.ID]; cancel != nil {
			cancel()
			continue
		}
		if job.Status == StatusQueued {
			interruptTOTPWorkflow(job, "流程已取消，后续步骤未执行")
			job.Status = StatusCanceled
			if !restoreCanceledSuboperation(job) {
				job.Prompt = "操作已取消"
			}
			job.OperationStatus, job.OperationError = "canceled", ""
			now := time.Now().UTC()
			job.UpdatedAt, job.FinishedAt = now, &now
			m.emitLocked(JobEvent{JobID: job.ID, Status: job.Status, Stage: "cancel", Prompt: job.Prompt, At: now})
		}
	}
	m.persistLocked()
	if m.lastPersistError != nil {
		m.mu.Unlock()
		return 0, ErrCancellationPersistence
	}
	m.mu.Unlock()
	if err := m.waitTrashRelease(ctx, emails, targets); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.trashMutationAllowedLocked(ctx); err != nil {
		return 0, err
	}
	entries := make(map[string]*TrashEntry, len(emails))
	previousPause := make(map[string][2]bool, len(targets))
	for _, job := range targets {
		if err := m.archiveRetainedLogsLocked(job, ctx); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, errors.New("account logs could not be safely archived; account remains active")
		}
		previousPause[job.ID] = [2]bool{job.RecoveryPaused, job.AutoRepairSyncPaused}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	for _, job := range targets {
		job.RecoveryPaused, job.AutoRepairSyncPaused = true, true
		key := emailKey(job.Email)
		entry := entries[key]
		if entry == nil {
			entry = &TrashEntry{ID: newID(), Email: key, DeletedAt: now, State: TrashStateTrashed}
			entries[key] = entry
		}
		entry.Jobs = append(entry.Jobs, job)
		delete(m.jobs, job.ID)
	}
	for _, entry := range entries {
		m.trash[entry.ID] = entry
	}
	m.persistLocked()
	if m.lastPersistError != nil {
		if errors.Is(m.lastPersistError, store.ErrDurabilityUncertain) {
			return len(entries), ErrTrashUncertain
		}
		for _, entry := range entries {
			delete(m.trash, entry.ID)
			for _, job := range entry.Jobs {
				job.RecoveryPaused, job.AutoRepairSyncPaused = previousPause[job.ID][0], previousPause[job.ID][1]
				m.jobs[job.ID] = job
			}
		}
		return 0, ErrTrashPersistence
	}
	return len(entries), nil
}

func (m *Manager) waitTrashRelease(ctx context.Context, emails map[string]bool, jobs []*Job) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		busy := false
		for key := range emails {
			if m.accountQueries[key] != nil {
				busy = true
			}
		}
		for _, job := range jobs {
			if m.active[job.ID] != nil {
				busy = true
			}
		}
		m.mu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.rootCtx.Done():
			return m.rootCtx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) selectTrashLocked(ctx context.Context, ids []string, restoring bool) ([]*TrashEntry, map[string]bool, error) {
	if err := m.trashMutationAllowedLocked(ctx); err != nil {
		return nil, nil, err
	}
	selected := make([]*TrashEntry, 0, len(ids))
	emails, seen := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		entry, ok := m.trash[id]
		if !ok {
			return nil, nil, ErrTrashNotFound
		}
		if restoring && entry.State != TrashStateTrashed {
			return nil, nil, ErrTrashPurging
		}
		key := emailKey(entry.Email)
		if m.deleting[key] || m.importing[key] || m.emailUse[key] != "" || m.accountQueries[key] != nil {
			return nil, nil, ErrTrashBusy
		}
		for _, job := range m.jobs {
			if emailKey(job.Email) == key {
				return nil, nil, ErrTrashConflict
			}
		}
		for _, job := range entry.Jobs {
			if m.jobs[job.ID] != nil {
				return nil, nil, ErrTrashConflict
			}
		}
		for otherID, other := range m.trash {
			if otherID != entry.ID && emailKey(other.Email) == key {
				return nil, nil, ErrTrashConflict
			}
		}
		emails[key] = true
		selected = append(selected, entry)
	}
	return selected, emails, nil
}

// RestoreTrash restores visibility only. It never reads/writes credentials,
// prepares recovery artifacts, queues jobs, or repeats an earlier operation.
func (m *Manager) RestoreTrash(ctx context.Context, ids []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _, err := m.selectTrashLocked(ctx, ids, true)
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	for _, entry := range entries {
		for _, job := range entry.Jobs {
			m.jobs[job.ID] = job
		}
		delete(m.trash, entry.ID)
	}
	m.persistLocked()
	if m.lastPersistError != nil {
		if errors.Is(m.lastPersistError, store.ErrDurabilityUncertain) {
			return len(entries), ErrTrashUncertain
		}
		for _, entry := range entries {
			m.trash[entry.ID] = entry
			for _, job := range entry.Jobs {
				delete(m.jobs, job.ID)
			}
		}
		return 0, ErrTrashPersistence
	}
	return len(entries), nil
}

// PurgeTrash commits an irreversible, retryable tombstone before deleting any
// local data. A failure retains that marker and can never make files live again.
func (m *Manager) PurgeTrash(ctx context.Context, ids []string) (int, error) {
	m.mu.Lock()
	entries, emails, err := m.selectTrashLocked(ctx, ids, false)
	if err != nil || len(entries) == 0 {
		m.mu.Unlock()
		return 0, err
	}
	before := make(map[string]TrashEntry, len(entries))
	now := time.Now().UTC()
	for _, entry := range entries {
		before[entry.ID] = *entry
		entry.State, entry.LastError = TrashStatePurging, ""
		if entry.PurgeStartedAt == nil {
			at := now
			entry.PurgeStartedAt = &at
		}
	}
	m.persistLocked()
	if m.lastPersistError != nil {
		if errors.Is(m.lastPersistError, store.ErrDurabilityUncertain) {
			m.mu.Unlock()
			return 0, ErrTrashUncertain
		}
		for _, entry := range entries {
			*entry = before[entry.ID]
		}
		m.mu.Unlock()
		return 0, ErrTrashPersistence
	}
	for key := range emails {
		m.deleting[key] = true
	}
	hook, vault := m.trashPurgeHook, m.vault
	m.mu.Unlock()
	defer m.releaseTrashGates(emails)
	purged := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return purged, m.recordPurgeFailure(entry, err)
		}
		directories, err := m.trashDirectories(ctx, entry)
		if err != nil {
			return purged, m.recordPurgeFailure(entry, err)
		}
		if err := m.retainPurgeDirectories(entry, directories); err != nil {
			return purged, m.recordPurgeFailure(entry, err)
		}
		if hook != nil {
			if err := hook(ctx, publicTrashEntry(entry)); err != nil {
				if ctx.Err() != nil {
					return purged, m.recordPurgeFailure(entry, ctx.Err())
				}
				return purged, m.recordPurgeFailure(entry, trashHookFailure(err))
			}
		}
		for _, id := range directories {
			if err := ctx.Err(); err != nil {
				return purged, m.recordPurgeFailure(entry, err)
			}
			if !safeID(id) {
				return purged, m.recordPurgeFailure(entry, ErrTrashInspection)
			}
			if m.store != nil {
				if err := os.RemoveAll(filepath.Join(m.store.Root, id)); err != nil {
					return purged, m.recordPurgeFailure(entry, TrashCleanupFailure(ErrTrashDirectoryCleanup, err))
				}
			}
		}
		if vault != nil {
			if err := credentials.DeleteContext(ctx, vault, entry.Email); err != nil && !errors.Is(err, os.ErrNotExist) {
				if ctx.Err() != nil {
					return purged, m.recordPurgeFailure(entry, ctx.Err())
				}
				return purged, m.recordPurgeFailure(entry, TrashCleanupFailure(ErrTrashCredentialCleanup, err))
			}
		}
		m.mu.Lock()
		delete(m.trash, entry.ID)
		m.persistLocked()
		if m.lastPersistError != nil {
			m.trash[entry.ID] = entry
			if errors.Is(m.lastPersistError, store.ErrDurabilityUncertain) {
				entry.LastError = ErrTrashUncertain.Error()
				m.mu.Unlock()
				return purged, ErrTrashUncertain
			}
			entry.LastError = ErrTrashPersistence.Error()
			m.mu.Unlock()
			return purged, ErrTrashPersistence
		}
		m.mu.Unlock()
		purged++
	}
	return purged, nil
}

// Make additional historical IDs visible to external local cleanup hooks and
// retain their ownership durably if any later deletion step fails. These IDs
// join only an already irreversible purge, never a restorable trash entry.
func (m *Manager) retainPurgeDirectories(entry *TrashEntry, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Directory inspection deliberately runs without mu. Recheck ownership
	// in the same critical section that adds each ID to the durable marker;
	// an explicit-ID import may have acquired it since inspection finished.
	for _, id := range ids {
		if m.jobs[id] != nil {
			return ErrTrashConflict
		}
		for otherID, other := range m.trash {
			if otherID == entry.ID {
				continue
			}
			for _, job := range other.Jobs {
				if job.ID == id {
					return ErrTrashConflict
				}
			}
		}
	}
	known := make(map[string]bool, len(entry.Jobs))
	for _, job := range entry.Jobs {
		known[job.ID] = true
	}
	previous := entry.Jobs
	for _, id := range ids {
		if !known[id] {
			entry.Jobs = append(entry.Jobs, &Job{ID: id, Email: entry.Email})
			known[id] = true
		}
	}
	if len(previous) == len(entry.Jobs) {
		return nil
	}
	m.persistLocked()
	if m.lastPersistError != nil {
		if errors.Is(m.lastPersistError, store.ErrDurabilityUncertain) {
			return ErrTrashUncertain
		}
		entry.Jobs = previous
		return ErrTrashPersistence
	}
	return nil
}

func (m *Manager) recordPurgeFailure(entry *TrashEntry, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.LastError = safeError(err)
	m.persistLocked()
	if m.lastPersistError != nil {
		if errors.Is(m.lastPersistError, store.ErrDurabilityUncertain) {
			return errors.Join(err, ErrTrashUncertain)
		}
		return errors.Join(err, ErrTrashPersistence)
	}
	return err
}

// Older CLI runs may have left another private directory for the same email
// before it was indexed. Include it in permanent cleanup, otherwise removing
// the final tombstone would let directory discovery revive that account.
func (m *Manager) trashDirectories(ctx context.Context, entry *TrashEntry) ([]string, error) {
	ids := make(map[string]bool, len(entry.Jobs))
	for _, job := range entry.Jobs {
		ids[job.ID] = true
	}
	if m.store != nil {
		root, err := os.Lstat(m.store.Root)
		if err != nil {
			return nil, TrashCleanupFailure(ErrTrashInspection, err)
		}
		if !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
			return nil, ErrTrashInspection
		}
		directories, err := os.ReadDir(m.store.Root)
		if err != nil {
			return nil, TrashCleanupFailure(ErrTrashInspection, err)
		}
		for _, directory := range directories {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			id := directory.Name()
			if ids[id] || !directory.IsDir() || directory.Type()&os.ModeSymlink != 0 || !safeID(id) {
				continue
			}
			raw, err := readRegularJSON(filepath.Join(m.store.Root, id, "job-meta.json"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, TrashCleanupFailure(ErrTrashInspection, err)
			}
			email := ""
			if job, ok := decodeLegacyJob(raw, id); ok {
				email = job.Email
			} else {
				var output, grantAvailable bool
				email, output, grantAvailable = recoveryArtifacts(m.store.Root, id)
				// Match startup discovery's TOTP-only recovery identity. Leaving
				// this journal behind would rediscover the account after its
				// final tombstone disappears. The reader rejects symlink files.
				if email == "" || (!output && !grantAvailable) {
					if raw, readErr := readRegularJSON(filepath.Join(m.store.Root, id, "totp-setup-result.json")); readErr == nil {
						var result map[string]any
						if json.Unmarshal(raw, &result) == nil && resultBool(result, "reset_totp_requested") {
							email = resultString(result, "email")
						}
					}
				}
			}
			if emailKey(email) == emailKey(entry.Email) {
				ids[id] = true
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]string, 0, len(ids))
	for id := range ids {
		if m.jobs[id] != nil {
			return nil, ErrTrashConflict
		}
		for otherID, other := range m.trash {
			if otherID == entry.ID {
				continue
			}
			for _, job := range other.Jobs {
				if job.ID == id {
					return nil, ErrTrashConflict
				}
			}
		}
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func trashMetadata(entry *TrashEntry) map[string]any {
	jobs := make([]map[string]any, 0, len(entry.Jobs))
	for _, job := range entry.Jobs {
		jobs = append(jobs, jobMetadata(job))
	}
	return map[string]any{"id": entry.ID, "email": entry.Email, "deletedAt": entry.DeletedAt, "purgeStartedAt": entry.PurgeStartedAt, "state": entry.State, "lastError": entry.LastError, "jobs": jobs}
}

// decodeTrash fails closed on malformed tombstones: startup must not rediscover
// retained private files into the active account list.
func decodeTrash(raw json.RawMessage) (map[string]*TrashEntry, map[string]bool, map[string]bool, error) {
	entries, ids, emails := map[string]*TrashEntry{}, map[string]bool{}, map[string]bool{}
	if len(raw) == 0 {
		return entries, ids, emails, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil, errors.New("invalid null trash metadata")
	}
	var values []struct {
		ID             string            `json:"id"`
		Email          string            `json:"email"`
		DeletedAt      time.Time         `json:"deletedAt"`
		PurgeStartedAt *time.Time        `json:"purgeStartedAt"`
		State          string            `json:"state"`
		LastError      string            `json:"lastError"`
		Jobs           []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, nil, nil, errors.New("invalid trash metadata")
	}
	for _, value := range values {
		key := emailKey(value.Email)
		if !safeID(value.ID) || key == "" || value.DeletedAt.IsZero() || len(value.Jobs) == 0 || entries[value.ID] != nil || emails[key] || (value.State != TrashStateTrashed && value.State != TrashStatePurging) || (value.State == TrashStatePurging && value.PurgeStartedAt == nil) {
			return nil, nil, nil, errors.New("invalid trash entry")
		}
		entry := &TrashEntry{ID: value.ID, Email: key, DeletedAt: value.DeletedAt, PurgeStartedAt: value.PurgeStartedAt, State: value.State, LastError: safeError(errors.New(value.LastError))}
		for _, rawJob := range value.Jobs {
			job, ok := decodeLegacyJob(rawJob, "")
			if !ok || !safeID(job.ID) || emailKey(job.Email) != key || ids[job.ID] {
				return nil, nil, nil, errors.New("invalid retained trash job")
			}
			job.Input, job.MailAPIURL = nil, ""
			entry.Jobs = append(entry.Jobs, job)
			ids[job.ID] = true
		}
		entries[entry.ID], emails[key] = entry, true
	}
	return entries, ids, emails, nil
}
