package accountquality

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/store"
)

type flight struct {
	id, key, accountRevision, checkID, source, executor, identity string
	settings                                                      Settings
	ctx                                                           context.Context
	cancel                                                        context.CancelFunc
	done                                                          chan struct{}
	doneOnce                                                      sync.Once
	running                                                       bool
	probed                                                        bool
	result                                                        *modelprobe.Result
	err                                                           error
}

type Service struct {
	mu                           sync.Mutex
	writeGate, runGate           chan struct{}
	provider                     Provider
	disk                         *store.Store
	persist                      func(context.Context, string, any) error
	settings                     Settings
	settingsErr                  error
	hasSettingsFile, writeFailed bool
	rows                         map[string]storedRow
	flights                      map[string]*flight
	pending                      []*flight
	active                       int
	version, sequence            uint64
	ctx                          context.Context
	cancel                       context.CancelFunc
	closed                       bool
	wake                         chan struct{}
	wg                           sync.WaitGroup
	now                          func() time.Time
	lastError                    string
}

func New(root string, p Provider) (*Service, error) {
	if root == "" || p.Accounts == nil || p.Acquire == nil {
		return nil, errors.New("模型质量检测服务配置不完整")
	}
	disk, err := store.New(filepath.Join(root, directoryName))
	if err != nil {
		return nil, ErrSettingsUnavailable
	}
	s := &Service{provider: p, disk: disk, persist: disk.WriteJSON, settings: DefaultSettings(), rows: map[string]storedRow{}, flights: map[string]*flight{}, writeGate: make(chan struct{}, 1), runGate: make(chan struct{}, 1), wake: make(chan struct{}, 1), now: time.Now, version: 1}
	s.reloadPolicyLocked()
	accounts := accountMap(p.Accounts())
	retained := s.retained(accounts)
	for id := range retained {
		var row storedRow
		err := readPrivateJSON(disk.Root, snapshotFile(id), &row)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || row.SchemaVersion != 1 || row.Snapshot.ID != id {
			row = storedRow{SchemaVersion: 1, Snapshot: Snapshot{ID: id, Stale: true, LastAttempt: Attempt{Status: "failed", ErrorCode: "cache_unavailable", Message: "历史检测结果无法读取，原文件保留"}}}
		} else {
			backfillHTTPObservation(&row, accounts[id], accounts[id].ID != "", s.now().UTC())
			row.Snapshot.ProbeHistory = normalizeProbeHistory(row.Snapshot.ProbeHistory)
			backfillProbeHistory(&row.Snapshot, s.now().UTC())
			row.Snapshot.HTTPObservation = nil
			row.Snapshot.Persisted = true
			if row.Snapshot.LastAttempt.Status == "queued" || row.Snapshot.LastAttempt.Status == "running" {
				row.Snapshot.LastAttempt.Status = "cancelled"
				row.Snapshot.LastAttempt.ErrorCode = "interrupted"
				row.Snapshot.LastAttempt.Message = "上次检测已随服务停止中断，不会自动重放"
				row.Snapshot.Persisted = false
			}
		}
		s.rows[id] = row
	}
	return s, nil
}

func accountMap(list []Account) map[string]Account {
	out := map[string]Account{}
	for _, a := range list {
		if a.ID != "" {
			if a.Key == "" {
				a.Key = a.ID
			}
			out[a.ID] = a
		}
	}
	return out
}
func (s *Service) retained(active map[string]Account) map[string]bool {
	out := map[string]bool{}
	for id := range active {
		out[id] = true
	}
	if s.provider.RetainedIDs != nil {
		for _, id := range s.provider.RetainedIDs() {
			if id != "" {
				out[id] = true
			}
		}
	}
	return out
}

// Caller owns writeGate; no Provider callback is made while mu is held.
func (s *Service) reloadPolicyLocked() {
	c, err := readSettingsFile(s.disk.Root)
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) && !s.hasSettingsFile && !s.writeFailed {
		return
	}
	if err != nil || validateSettings(c) != nil {
		s.settingsErr = ErrSettingsUnavailable
		s.cancelAllLocked("检测设置不可读取，检测已停止")
		return
	}
	c.PersistenceWarning = ""
	if s.hasSettingsFile && (c.Revision < s.settings.Revision || c.Revision == s.settings.Revision && c != s.settings) {
		s.settingsErr = ErrSettingsUnavailable
		s.cancelAllLocked("检测设置版本冲突，检测已停止")
		return
	}
	if c != s.settings {
		s.cancelAllLocked("检测设置已改变，请按新设置检测")
		s.version++
	}
	s.settings = c
	s.hasSettingsFile = true
	if !s.writeFailed {
		s.settingsErr = nil
	}
}

func (s *Service) Settings() (Settings, error) {
	return s.SettingsContext(context.Background())
}

// SettingsContext lets a caller cancel waiting for a policy writer. It cannot
// interrupt an operating-system file read that has already started.
func (s *Service) SettingsContext(ctx context.Context) (Settings, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Settings{}, err
	}
	return s.settingsContext(ctx)
}

func (s *Service) settingsContext(ctx context.Context) (Settings, error) {
	if err := s.lockWrite(ctx); err != nil {
		return Settings{}, err
	}
	defer s.unlockWrite()
	if err := ctx.Err(); err != nil {
		return Settings{}, err
	}
	s.reloadPolicyLocked()
	if err := ctx.Err(); err != nil {
		return Settings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.settings
	if s.settingsErr != nil {
		c.PersistenceWarning = s.settingsErr.Error()
	}
	return c, s.settingsErr
}

func (s *Service) Update(ctx context.Context, c Settings) (Settings, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.Model = strings.TrimSpace(c.Model)
	c.PersistenceWarning = ""
	if err := validateSettings(c); err != nil {
		return c, err
	}
	if err := s.lockWrite(ctx); err != nil {
		return c, err
	}
	defer s.unlockWrite()
	s.reloadPolicyLocked()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return c, ErrClosed
	}
	if c.Revision != s.settings.Revision {
		s.mu.Unlock()
		return c, ErrConflict
	}
	if c.Revision == ^uint64(0) {
		s.mu.Unlock()
		return c, ErrConflict
	}
	c.Revision++
	s.mu.Unlock()
	err := s.persist(ctx, settingsName, c)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.writeFailed = true
		s.settingsErr = ErrPersistence
		if errors.Is(err, store.ErrDurabilityUncertain) {
			s.settings = c
			s.hasSettingsFile = true
			s.settingsErr = ErrPersistenceUncertain
		}
		s.cancelAllLocked("检测设置保存未确认，检测已停止")
		s.version++
		out := s.settings
		out.PersistenceWarning = s.settingsErr.Error()
		return out, s.settingsErr
	}
	s.settings = c
	s.hasSettingsFile = true
	s.writeFailed = false
	s.settingsErr = nil
	s.version++
	s.cancelAllLocked("检测设置已改变，请按新设置检测")
	return c, nil
}

func (s *Service) Start(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	s.mu.Lock()
	if s.closed || s.ctx != nil {
		s.mu.Unlock()
		return
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.ctx.Done():
				s.mu.Lock()
				s.cancelAllLocked("检测服务已停止")
				s.mu.Unlock()
				return
			case <-s.wake:
			}
			for {
				s.mu.Lock()
				if s.closed || s.ctx.Err() != nil || len(s.pending) == 0 {
					s.mu.Unlock()
					break
				}
				f := s.pending[0]
				s.pending = s.pending[1:]
				s.mu.Unlock()
				s.execute(f, nil)
			}
		}
	}()
}

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.closed = true
	s.cancelAllLocked("检测服务已关闭")
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	if err := s.lockWrite(ctx); err != nil {
		return err
	}
	s.unlockWrite()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) newFlightLocked(a Account, c Settings, source string) *flight {
	s.sequence++
	ctx, cancel := context.WithCancel(s.ctx)
	f := &flight{id: a.ID, key: a.Key, accountRevision: a.Revision, checkID: fmt.Sprintf("quality-%d-%d", s.now().UnixNano(), s.sequence), source: source, executor: source, settings: c, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	s.flights[a.Key] = f
	row := s.rows[a.ID]
	row.SchemaVersion = 1
	row.AccountRevision = a.Revision
	row.Snapshot.ID = a.ID
	row.Snapshot.LastAttempt = Attempt{CheckID: f.checkID, Source: source, Status: "queued"}
	row.Snapshot.Persisted = false
	s.rows[a.ID] = row
	s.version++
	return f
}

func (s *Service) Refresh(ctx context.Context, ids []string, all bool) (Receipt, error) {
	return s.RefreshWithOptions(ctx, ids, all, nil)
}

func (s *Service) RefreshWithOptions(ctx context.Context, ids []string, all bool, selected *RunOptions) (Receipt, error) {
	out := Receipt{Accepted: []string{}, Merged: []string{}, Skipped: []Skip{}}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if all && len(ids) > 0 || !all && (len(ids) == 0 || len(ids) > 1000) {
		return out, errors.New("请选择1–1000个账号，或单独使用全部账号")
	}
	if _, err := s.settingsContext(ctx); err != nil {
		return out, err
	}
	accounts := accountMap(s.provider.Accounts())
	if all {
		ids = nil
		for id := range accounts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx == nil || s.ctx.Err() != nil {
		return out, ErrClosed
	}
	if s.settingsErr != nil {
		return out, s.settingsErr
	}
	if !s.settings.Enabled {
		return out, ErrDisabled
	}
	runSettings, err := ApplyRunOptions(s.settings, selected)
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	now := s.now().UTC()
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		a, ok := accounts[id]
		if !ok {
			out.Skipped = append(out.Skipped, Skip{id, "账号不存在"})
			continue
		}
		if f := s.flights[a.Key]; f != nil {
			if optionsHash(f.settings) != optionsHash(runSettings) {
				out.Skipped = append(out.Skipped, Skip{id, "账号已有其他模型或推理强度的检测，请等待结束"})
				continue
			}
			out.Merged = append(out.Merged, id)
			if out.CheckID == "" {
				out.CheckID = f.checkID
			}
			continue
		}
		if !a.CanCheck {
			reason := a.Reason
			if reason == "" {
				reason = "当前账号暂不可检测"
			}
			out.Skipped = append(out.Skipped, Skip{id, reason})
			continue
		}
		if len(s.flights) >= 1000 {
			out.Skipped = append(out.Skipped, Skip{id, "检测队列已满"})
			continue
		}
		row := s.rows[id]
		if row.AccountRevision == a.Revision && row.Snapshot.NextAllowedAt != nil && now.Before(*row.Snapshot.NextAllowedAt) {
			out.Skipped = append(out.Skipped, Skip{id, "检测仍在冷却"})
			continue
		}
		f := s.newFlightLocked(a, runSettings, "manual")
		s.pending = append(s.pending, f)
		out.Accepted = append(out.Accepted, id)
		if out.CheckID == "" {
			out.CheckID = f.checkID
		}
	}
	s.notify()
	return out, nil
}

func (s *Service) cancelFlightLocked(f *flight, message string) {
	f.cancel()
	if f.running {
		return
	}
	if s.flights[f.key] != f {
		return
	}
	delete(s.flights, f.key)
	row := s.rows[f.id]
	if row.Snapshot.LastAttempt.CheckID == f.checkID {
		now := s.now().UTC()
		row.Snapshot.LastAttempt.Status = "cancelled"
		row.Snapshot.LastAttempt.FinishedAt = &now
		row.Snapshot.LastAttempt.ErrorCode = "cancelled"
		row.Snapshot.LastAttempt.Message = message
		row.Snapshot.Persisted = false
		s.rows[f.id] = row
	}
	f.err = context.Canceled
	f.doneOnce.Do(func() { close(f.done) })
	s.version++
}
func (s *Service) cancelAllLocked(message string) {
	for _, f := range s.flights {
		s.cancelFlightLocked(f, message)
	}
	s.pending = nil
}
func (s *Service) Cancel(ctx context.Context, ids []string) (CancelReceipt, error) {
	out := CancelReceipt{Cancelled: []string{}, Unchanged: []string{}}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if len(ids) == 0 || len(ids) > 1000 {
		return out, errors.New("请选择1–1000个账号")
	}
	accounts := accountMap(s.provider.Accounts())
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		a := accounts[id]
		if a.Key == "" {
			a.Key = id
		}
		f := s.flights[a.Key]
		if f == nil {
			for _, candidate := range s.flights {
				if candidate.id == id {
					f = candidate
					break
				}
			}
		}
		if f != nil {
			s.cancelFlightLocked(f, "检测已取消")
			out.Cancelled = append(out.Cancelled, id)
		} else {
			out.Unchanged = append(out.Unchanged, id)
		}
	}
	kept := s.pending[:0]
	for _, f := range s.pending {
		if s.flights[f.key] == f {
			kept = append(kept, f)
		}
	}
	s.pending = kept
	return out, nil
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := 0
	for _, f := range s.flights {
		if !f.running {
			queued++
		}
	}
	out := Status{Enabled: s.settings.Enabled, BeforeUpload: s.settings.BeforeUpload, Running: s.ctx != nil && !s.closed && s.ctx.Err() == nil, Queued: queued, Active: s.active, SettingsAvailable: s.settingsErr == nil, SettingsRevision: s.settings.Revision, LastError: s.lastError, Revision: s.version}
	if s.settingsErr != nil {
		out.LastError = s.settingsErr.Error()
	}
	return out
}

func (s *Service) Snapshots(ids []string) (map[string]Snapshot, uint64) {
	accounts := accountMap(s.provider.Accounts())
	retained := s.retained(accounts)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Snapshot{}
	now := s.now().UTC()
	policyOptionsHash := optionsHash(s.settings)
	for _, id := range ids {
		a, active := accounts[id]
		if !active && !retained[id] {
			continue
		}
		row := s.rows[id]
		snap := cloneSnapshot(row.Snapshot)
		snap.ID = id
		snap.Revision = s.version
		snap.HTTPObservation = nil
		if active && a.CanCheck && row.HTTPObservationRevision == a.Revision {
			snap.HTTPObservation = cloneHTTPObservation(row.HTTPObservation)
		}
		if snap.LastAttempt.Status == "" {
			snap.LastAttempt.Status = "never"
		}
		expired := snap.LastResult != nil && now.Sub(snap.LastResult.CheckedAt) > time.Duration(s.settings.ResultTTLSeconds)*time.Second
		snap.Stale = snap.LastResult == nil || expired || row.OptionsHash != policyOptionsHash || !active || row.AccountRevision != a.Revision
		// A one-off model selection is independent of the default upload policy;
		// it must not be described as time-expired immediately after completion.
		// Recompute this projection rather than trusting any stored reason.
		snap.StaleReason = ""
		if snap.Stale && snap.LastResult != nil {
			switch {
			case expired:
				snap.StaleReason = "expired"
			case !active:
				snap.StaleReason = "account_unavailable"
			case row.AccountRevision != a.Revision:
				snap.StaleReason = "authorization_changed"
			case snap.LastResult.RuleVersion != modelprobe.RuleVersion:
				snap.StaleReason = "rule_changed"
			case row.OptionsHash != policyOptionsHash:
				snap.StaleReason = "options_changed"
			}
		}
		snap.State = "unavailable"
		snap.CanCheck = false
		snap.UnavailableReason = "当前账号暂不可检测"
		if active {
			switch {
			case s.flights[a.Key] != nil:
				if s.flights[a.Key].running {
					snap.State = "active"
				} else {
					snap.State = "queued"
				}
				snap.UnavailableReason = "账号已有检测任务"
			case s.closed || s.ctx == nil || s.ctx.Err() != nil:
				snap.UnavailableReason = ErrClosed.Error()
			case s.settingsErr != nil:
				snap.UnavailableReason = ErrSettingsUnavailable.Error()
			case !s.settings.Enabled:
				snap.UnavailableReason = ErrDisabled.Error()
			case !a.CanCheck:
				if a.Reason != "" {
					snap.UnavailableReason = a.Reason
				}
			case row.AccountRevision == a.Revision && snap.NextAllowedAt != nil && now.Before(*snap.NextAllowedAt):
				snap.State = "coolingDown"
				snap.UnavailableReason = "检测仍在冷却"
			default:
				snap.State = "eligible"
				snap.CanCheck = true
				snap.UnavailableReason = ""
			}
		}
		out[id] = snap
	}
	return out, s.version
}
