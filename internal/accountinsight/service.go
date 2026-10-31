package accountinsight

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/store"
)

type ticket struct {
	id, queryID string
	revision    uint64
	automatic   bool
	cancel      context.CancelFunc
}
type Service struct {
	mu                sync.Mutex
	writeMu           sync.Mutex
	settings          Settings
	provider          Provider
	disk              *store.Store
	persist           func(context.Context, string, any) error
	snapshots         map[string]storedSnapshot
	pending           []*ticket
	flights           map[string]*ticket
	active            int
	version, sequence uint64
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
	wake              chan struct{}
	wg                sync.WaitGroup
	closed            bool
	lastError         string
	now               func() time.Time
	tick, budget      time.Duration
	admissionInterval time.Duration
	nextAdmission     time.Time
}

func New(root string, p Provider) (*Service, error) {
	if root == "" || p.Accounts == nil || p.Acquire == nil {
		return nil, errors.New("账号查询服务配置不完整")
	}
	disk, err := store.New(filepath.Join(root, "account-insights"))
	if err != nil {
		return nil, errors.New("账号查询存储不可用")
	}
	s := &Service{provider: p, disk: disk, settings: DefaultSettings(), snapshots: map[string]storedSnapshot{}, flights: map[string]*ticket{}, wake: make(chan struct{}, 1), now: time.Now, tick: time.Second, budget: 30 * time.Second, admissionInterval: time.Second}
	s.persist = disk.WriteJSON
	var saved Settings
	if err = s.read("settings.json", &saved); err == nil {
		if validateSettings(saved) == nil {
			s.settings = saved
		} else {
			s.lastError = "账号查询设置版本或内容无效，已安全关闭；原文件保留"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		s.lastError = "账号查询设置读取失败，已安全关闭；原文件保留"
	}
	for _, a := range p.Accounts() {
		var row storedSnapshot
		if s.read(snapshotFile(a.ID), &row) == nil && row.SchemaVersion == 1 && row.Snapshot.ID == a.ID {
			row.Snapshot.Stale = true
			if row.Snapshot.LastAttempt.Status == "running" || row.Snapshot.LastAttempt.Status == "queued" {
				row.Snapshot.LastAttempt.Status = "cancelled"
				row.Snapshot.LastAttempt.Message = "上次查询已随服务停止中断"
			}
			s.snapshots[a.ID] = row
		}
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	// PruneDeleted owns its recoverable warning and preserves configuration
	// warnings; a later successful cleanup can clear only its own warning.
	_ = s.PruneDeleted(cleanupCtx)
	cancelCleanup()
	return s, nil
}
func (s *Service) read(name string, out any) error {
	st, err := os.Lstat(s.disk.Path(name))
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("query state is not a regular file")
	}
	f, err := os.Open(s.disk.Path(name))
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(st, opened) || !opened.Mode().IsRegular() || opened.Size() > 4<<20 {
		return errors.New("query state changed or exceeds size limit")
	}
	if err := privatefile.Verify(f); err != nil {
		return err
	}
	d := json.NewDecoder(io.LimitReader(f, (4<<20)+1))
	if err := d.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return errors.New("query state has trailing or invalid JSON")
	}
	return nil
}
func snapshotFile(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:]) + ".json"
}
func validateSettings(c Settings) error {
	if c.SchemaVersion != 1 {
		return errors.New("不支持的查询配置版本")
	}
	if c.IntervalMinutes < 1 || c.IntervalMinutes > 1440 {
		return errors.New("更新间隔必须是 1–1440 分钟的整数")
	}
	if len(c.ExcludedAccountIDs) > 10000 {
		return errors.New("排除账号数量超过上限")
	}
	for _, id := range c.ExcludedAccountIDs {
		if strings.TrimSpace(id) == "" || len(id) > 256 {
			return errors.New("排除账号标识无效")
		}
	}
	return nil
}
func clone[T any](v T) T { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }
func (s *Service) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := clone(s.settings)
	if s.lastError != "" {
		c.PersistenceWarning = s.lastError
	}
	return c
}
func (s *Service) Update(ctx context.Context, c Settings) (Settings, error) {
	if err := validateSettings(c); err != nil {
		return s.Settings(), err
	}
	c.ExcludedAccountIDs = append([]string{}, c.ExcludedAccountIDs...)
	sort.Strings(c.ExcludedAccountIDs)
	unique := c.ExcludedAccountIDs[:0]
	for _, id := range c.ExcludedAccountIDs {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	c.ExcludedAccountIDs = unique
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return s.Settings(), ErrClosed
	}
	if c.Revision != s.settings.Revision {
		s.mu.Unlock()
		return s.Settings(), ErrConflict
	}
	c.Revision++
	c.PersistenceWarning = ""
	s.mu.Unlock()
	err := s.persist(ctx, "settings.json", c)
	var visible Settings
	uncertain := errors.Is(err, store.ErrDurabilityUncertain)
	readBack := uncertain && s.read("settings.json", &visible) == nil && validateSettings(visible) == nil
	s.mu.Lock()
	if err != nil {
		s.lastError = "查询设置未保存，请核对存储后重试"
		if uncertain {
			// Reconcile the visible version after rename, without claiming crash
			// durability or enabling collection on an ambiguous save.
			if readBack && visible.Revision >= s.settings.Revision {
				s.settings = visible
			}
			if s.settings.Revision < c.Revision {
				s.settings.Revision = c.Revision
			}
			s.settings.Enabled = false
			s.lastError = "配置文件持久性未确认，本次已停止；重启后的配置尚未确认，请重新读取并明确保存"
		}
		// A failed disable is fail-closed in this process, without claiming durable success.
		if !c.Enabled || uncertain {
			s.settings.Enabled = false
			s.cancelAllLocked("查询已停止，但配置保存尚未确认")
		}
		s.version++
		s.mu.Unlock()
		s.notify()
		if uncertain {
			return s.Settings(), ErrPersistenceUncertain
		}
		return s.Settings(), ErrPersistence
	}
	s.settings = c
	s.lastError = ""
	s.version++
	s.cancelAllLocked("查询配置已变更，请按新配置更新")
	s.rescheduleLocked()
	s.mu.Unlock()
	s.notify()
	return s.Settings(), nil
}
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) Start(parent context.Context) {
	s.mu.Lock()
	if s.ctx != nil || s.closed {
		s.mu.Unlock()
		return
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()
	go func() {
		defer close(done)
		timer := time.NewTicker(s.tick)
		defer timer.Stop()
		for {
			select {
			case <-s.ctx.Done():
				s.mu.Lock()
				s.cancelAllLocked("服务已停止")
				s.mu.Unlock()
				s.wg.Wait()
				return
			case <-timer.C:
			case <-s.wake:
			}
			s.dispatch()
		}
	}()
}
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	cancel, done := s.cancel, s.done
	s.cancelAllLocked("服务已停止")
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (s *Service) cancelAllLocked(reason string) {
	for _, t := range s.pending {
		s.finishQueuedLocked(t, reason)
	}
	s.pending = nil
	for _, t := range s.flights {
		if t.cancel != nil {
			t.cancel()
		}
	}
}
func (s *Service) finishQueuedLocked(t *ticket, reason string) {
	row := s.snapshots[t.id]
	row.Snapshot.LastAttempt.Status = "cancelled"
	row.Snapshot.LastAttempt.Message = reason
	n := s.now().UTC()
	row.Snapshot.LastAttempt.FinishedAt = &n
	row.Snapshot.Persisted = false
	next := n.Add(time.Duration(s.settings.IntervalMinutes) * time.Minute)
	row.Snapshot.NextCheckAt = &next
	s.snapshots[t.id] = row
	delete(s.flights, t.id)
}
func (s *Service) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.flights[id]
	if t == nil {
		return false
	}
	if t.cancel != nil {
		t.cancel()
	} else {
		for i, q := range s.pending {
			if q == t {
				s.pending = append(s.pending[:i], s.pending[i+1:]...)
				break
			}
		}
		s.finishQueuedLocked(t, "查询已取消")
	}
	s.version++
	s.notify()
	return true
}
func accountMap(list []Account) map[string]Account {
	m := make(map[string]Account, len(list))
	for _, a := range list {
		m[a.ID] = a
	}
	return m
}
func excluded(c Settings, id string) bool {
	for _, x := range c.ExcludedAccountIDs {
		if x == id {
			return true
		}
	}
	return false
}
func (s *Service) Refresh(ids []string) (Receipt, error) {
	out := Receipt{Accepted: []string{}, Merged: []string{}, Skipped: []Skip{}}
	if len(ids) == 0 || len(ids) > 1000 {
		return out, errors.New("请选择 1–1000 个账号")
	}
	accounts := accountMap(s.provider.Accounts())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx == nil || s.ctx.Err() != nil {
		return out, ErrClosed
	}
	if !s.settings.Enabled {
		return out, ErrDisabled
	}
	s.sequence++
	out.QueryID = fmt.Sprintf("q-%d-%d", s.now().UnixNano(), s.sequence)
	seen := map[string]bool{}
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
		if _, ok = s.flights[id]; ok {
			out.Merged = append(out.Merged, id)
			continue
		}
		if !a.CanQuery {
			out.Skipped = append(out.Skipped, Skip{id, a.Reason})
			continue
		}
		if len(s.flights) >= 1000 {
			out.Skipped = append(out.Skipped, Skip{id, "查询队列已满，请等待现有查询完成"})
			continue
		}
		row := s.snapshots[id]
		if row.AccountRevision == a.Revision && row.BlockedCode != "" {
			out.Skipped = append(out.Skipped, Skip{id, row.BlockedReason})
			continue
		}
		if row.AccountRevision == a.Revision && row.Snapshot.NextAllowedAt != nil && s.now().Before(*row.Snapshot.NextAllowedAt) {
			out.Skipped = append(out.Skipped, Skip{id, "查询冷却中，请稍后重试"})
			continue
		}
		s.enqueueLocked(a, out.QueryID, false)
		out.Accepted = append(out.Accepted, id)
	}
	s.notify()
	return out, nil
}
func (s *Service) enqueueLocked(a Account, qid string, automatic bool) {
	row := s.snapshots[a.ID]
	if row.AccountRevision != a.Revision {
		row = storedSnapshot{SchemaVersion: 1, AccountRevision: a.Revision, Snapshot: Snapshot{ID: a.ID, AuthorizationMethod: a.AuthorizationMethod}}
	}
	row.Snapshot.LastAttempt = Attempt{QueryID: qid, Status: "queued"}
	row.Snapshot.Persisted = false
	s.snapshots[a.ID] = row
	t := &ticket{id: a.ID, queryID: qid, revision: s.settings.Revision, automatic: automatic}
	s.flights[a.ID] = t
	s.pending = append(s.pending, t)
	s.version++
}
