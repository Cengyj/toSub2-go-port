// Package recyclebin owns the persisted retention policy and local cleanup loop.
// Account archival and irreversible deletion remain in the jobs transaction.
package recyclebin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

var ErrConflict = errors.New("回收站设置已被其他页面修改，请重新读取")
var ErrClosed = errors.New("回收站服务正在关闭")
var ErrUncertain = errors.New("回收站设置保存结果待核实，当前进程已暂停自动到期清理；请修复存储后重新确认保存，勿立即重启")
var ErrPersistence = errors.New("回收站设置未保存，请检查本地存储后重试")

type Settings struct {
	SchemaVersion      int    `json:"schemaVersion"`
	Revision           uint64 `json:"revision"`
	RetentionDays      int    `json:"retentionDays"`
	PersistenceWarning string `json:"persistenceWarning,omitempty"`
}
type Status struct {
	Running    bool       `json:"running"`
	LastRunAt  *time.Time `json:"lastRunAt,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
	LastPurged int        `json:"lastPurged"`
}

// Repository allows the scheduler to be exercised without any real accounts.
type Repository interface {
	ListTrash() []*jobs.TrashEntry
	PurgeTrash(context.Context, []string) (int, error)
}
type Service struct {
	mu             sync.Mutex
	gate           chan struct{}
	disk           *store.Store
	repo           Repository
	settings       Settings
	status         Status
	statusErrorIDs []string
	uncertain      bool
	closed         bool
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan struct{}
	writeSettings  func(context.Context, Settings) error
}

func New(root string, repo Repository) (*Service, error) {
	if repo == nil {
		return nil, errors.New("回收站仓库不可用")
	}
	disk, err := store.New(root)
	if err != nil {
		return nil, err
	}
	// Defaults apply only to a genuinely absent policy. Prepopulating these
	// fields before decoding would turn a missing/null schema or revision in
	// an existing document into a validated automatic-deletion policy.
	var settings Settings
	err = disk.ReadJSON(context.Background(), "recycle-bin-settings.json", &settings)
	if store.IsNotFound(err) {
		settings = Settings{SchemaVersion: 1, Revision: 1}
	} else if err != nil {
		return nil, fmt.Errorf("读取回收站设置失败: %w", err)
	}
	if settings.SchemaVersion != 1 || settings.Revision == 0 || settings.RetentionDays < 0 || settings.RetentionDays > 3650 {
		return nil, errors.New("回收站设置格式无效，未启用自动清理")
	}
	settings.PersistenceWarning = ""
	s := &Service{disk: disk, repo: repo, settings: settings, gate: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
	s.writeSettings = func(ctx context.Context, value Settings) error {
		return disk.WriteJSON(ctx, "recycle-bin-settings.json", value)
	}
	return s, nil
}
func (s *Service) Settings() Settings { s.mu.Lock(); defer s.mu.Unlock(); return s.settings }
func (s *Service) Status() Status {
	s.mu.Lock()
	out := s.status
	errorIDs := append([]string(nil), s.statusErrorIDs...)
	if out.LastRunAt != nil {
		at := *out.LastRunAt
		out.LastRunAt = &at
	}
	s.mu.Unlock()
	// Manual cleanup or restore can resolve a record failure before the next
	// scheduled sweep. Reconcile this read without rewriting newer status or
	// holding the service mutex while acquiring the repository's locks.
	if out.LastError != "" && len(errorIDs) > 0 {
		pending := make(map[string]bool, len(errorIDs))
		for _, id := range errorIDs {
			pending[id] = true
		}
		for _, entry := range s.repo.ListTrash() {
			if entry != nil && pending[entry.ID] {
				return out
			}
		}
		out.LastError = ""
	}
	return out
}
func (s *Service) lock(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) unlock() { <-s.gate }
func (s *Service) Update(ctx context.Context, requested Settings) (Settings, error) {
	if requested.SchemaVersion != 1 || requested.RetentionDays < 0 || requested.RetentionDays > 3650 {
		return s.Settings(), errors.New("保留天数必须是 0–3650 的整数，0 表示关闭自动清理")
	}
	if err := s.lock(ctx); err != nil {
		return s.Settings(), err
	}
	defer s.unlock()
	s.mu.Lock()
	if s.closed {
		out := s.settings
		s.mu.Unlock()
		return out, ErrClosed
	}
	if requested.Revision != s.settings.Revision {
		out := s.settings
		s.mu.Unlock()
		return out, ErrConflict
	}
	if requested.RetentionDays == s.settings.RetentionDays && !s.uncertain {
		out := s.settings
		s.mu.Unlock()
		return out, nil
	}
	if s.settings.Revision == ^uint64(0) {
		out := s.settings
		s.mu.Unlock()
		return out, ErrConflict
	}
	next := Settings{SchemaVersion: 1, Revision: s.settings.Revision + 1, RetentionDays: requested.RetentionDays}
	s.mu.Unlock()
	err := s.writeSettings(ctx, next)
	s.mu.Lock()
	if err != nil {
		if errors.Is(err, store.ErrDurabilityUncertain) {
			s.settings = next
			s.settings.PersistenceWarning = ErrUncertain.Error()
			s.uncertain = true
		}
		out := s.settings
		s.mu.Unlock()
		if errors.Is(err, store.ErrDurabilityUncertain) {
			return out, ErrUncertain
		}
		return out, ErrPersistence
	}
	s.settings = next
	s.uncertain = false
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return next, nil
}

// ExpiresAt deliberately uses elapsed UTC days, independent of request timezones.
// A policy change applies to existing entries from their original deletion time.
func ExpiresAt(entry *jobs.TrashEntry, days int) *time.Time {
	if entry == nil || days <= 0 || entry.DeletedAt.IsZero() {
		return nil
	}
	expiry := entry.DeletedAt.UTC().Add(time.Duration(days) * 24 * time.Hour)
	return &expiry
}

func sweepFailureMessage(err error) string {
	switch {
	case errors.Is(err, ErrUncertain):
		return ErrUncertain.Error()
	case errors.Is(err, jobs.ErrTrashUncertain):
		return jobs.ErrTrashUncertain.Error()
	case errors.Is(err, jobs.ErrTrashPersistence):
		return jobs.ErrTrashPersistence.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "本轮自动清理已超时，未完成的记录将在下次自动检查时继续处理"
	case errors.Is(err, context.Canceled):
		return "本轮自动清理已中断，未完成的记录仍保留，后续自动检查会继续处理"
	case errors.Is(err, jobs.ErrTrashInspection):
		return jobs.ErrTrashInspection.Error()
	case errors.Is(err, jobs.ErrTrashBusy):
		return "部分回收记录正由其他操作处理，将在下次自动检查时重试"
	case errors.Is(err, jobs.ErrTrashNotFound):
		return "回收记录已变化，请刷新列表核对后等待下次自动检查"
	case errors.Is(err, jobs.ErrTrashConflict):
		return jobs.ErrTrashConflict.Error()
	}
	for _, stage := range []error{jobs.ErrTrashInsightCleanup, jobs.ErrTrashQualityCleanup, jobs.ErrTrashSMSCleanup, jobs.ErrTrashLocalCleanup, jobs.ErrTrashDirectoryCleanup, jobs.ErrTrashCredentialCleanup} {
		if errors.Is(err, stage) {
			return stage.Error()
		}
	}
	return "回收站清理未完成；请查看记录详情，核对本地存储或凭据服务后重试"
}

func recordScopedSweepFailure(err error) bool {
	// Policy uncertainty, persistence, cancellation and inspection failures can
	// affect the whole service and must remain visible even with an empty list.
	for _, global := range []error{ErrUncertain, jobs.ErrTrashUncertain, jobs.ErrTrashPersistence, context.Canceled, context.DeadlineExceeded, jobs.ErrTrashInspection} {
		if errors.Is(err, global) {
			return false
		}
	}
	for _, local := range []error{jobs.ErrTrashBusy, jobs.ErrTrashNotFound, jobs.ErrTrashConflict, jobs.ErrTrashLocalCleanup, jobs.ErrTrashDirectoryCleanup, jobs.ErrTrashCredentialCleanup, jobs.ErrTrashInsightCleanup, jobs.ErrTrashQualityCleanup, jobs.ErrTrashSMSCleanup} {
		if errors.Is(err, local) {
			return true
		}
	}
	return false
}

func (s *Service) trashEntryExists(id string) bool {
	for _, entry := range s.repo.ListTrash() {
		if entry != nil && entry.ID == id {
			return true
		}
	}
	return false
}

func (s *Service) Sweep(ctx context.Context, now time.Time) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	policy := s.settings
	uncertain := s.uncertain
	s.status.Running = true
	s.mu.Unlock()
	purged := 0
	var sweepErr error
	var errorIDs, busyIDs []string
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		at := now.UTC()
		s.status.Running = false
		s.status.LastRunAt = &at
		s.status.LastPurged = purged
		s.status.LastError = ""
		s.statusErrorIDs = nil
		if sweepErr != nil {
			s.status.LastError = sweepFailureMessage(sweepErr)
			if recordScopedSweepFailure(sweepErr) {
				s.statusErrorIDs = append([]string(nil), errorIDs...)
			}
		}
	}()
	if uncertain {
		sweepErr = ErrUncertain
		return sweepErr
	}
	attempted := 0
	for _, entry := range s.repo.ListTrash() {
		if err := ctx.Err(); err != nil {
			sweepErr = err
			return err
		}
		// A committed purge is irreversible and resumable even if expiry is disabled.
		expiry := ExpiresAt(entry, policy.RetentionDays)
		if entry.State != "purging" && (expiry == nil || now.Before(*expiry)) {
			continue
		}
		if attempted >= 20 {
			break
		}
		attempted++
		count, err := s.repo.PurgeTrash(ctx, []string{entry.ID})
		purged += count
		if err != nil {
			if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
				err = errors.Join(err, ctx.Err())
			}
			if errors.Is(err, jobs.ErrTrashNotFound) && recordScopedSweepFailure(err) && !s.trashEntryExists(entry.ID) {
				continue
			}
			if errors.Is(err, jobs.ErrTrashBusy) && recordScopedSweepFailure(err) {
				busyIDs = append(busyIDs, entry.ID)
				continue
			}
			sweepErr = err
			errorIDs = []string{entry.ID}
			return err
		}
	}
	if len(busyIDs) > 0 {
		sweepErr = jobs.ErrTrashBusy
		errorIDs = busyIDs
		return sweepErr
	}
	return nil
}
func (s *Service) Start(parent context.Context) {
	s.mu.Lock()
	if s.closed || s.done != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()
	go func() {
		defer close(done)
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = s.Sweep(sweepCtx, time.Now())
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-s.wake:
			}
		}
	}()
}
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// The background loop is not the only owner of local side effects:
	// Update and an explicit Sweep also hold gate. Do not report a clean
	// shutdown while an already-admitted write or purge is still running.
	// closed prevents queued callers from starting work after this barrier.
	if err := s.lock(ctx); err != nil {
		return err
	}
	s.unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
