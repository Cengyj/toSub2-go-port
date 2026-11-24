// Package maintenance owns durable maintenance admission, independently of
// account metadata. Existing work may drain; no old operation is resurrected.
package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/privatefile"
)

var (
	ErrPaused      = errors.New("MAINTENANCE_ACTIVE: 服务正在维护，已暂停新操作；已受理任务仍可完成输入或取消")
	ErrConflict    = errors.New("MAINTENANCE_CONFLICT: 维护配置已变化，请重新读取")
	ErrUnavailable = errors.New("MAINTENANCE_UNAVAILABLE: 无法确认维护配置，已暂停新操作；原文件保留")
	ErrUncertain   = errors.New("MAINTENANCE_UNCERTAIN: 维护配置持久化未确认，当前进程继续暂停新操作")
)

type State struct {
	Schema   int   `json:"schema"`
	Revision int64 `json:"revision"`
	Enabled  bool  `json:"enabled"`
}
type Status struct {
	State
	ActiveRequests int    `json:"activeRequests"`
	Uncertain      bool   `json:"uncertain"`
	Error          string `json:"error,omitempty"`
}
type Coordinator struct {
	mu                sync.Mutex
	root              string
	state             State
	paused, uncertain bool
	loadErr           error
	active            int
	drainRelease      func() error
	write             func(context.Context, string, []byte) error
}

func New(root string) *Coordinator {
	c := &Coordinator{root: root, state: State{Schema: 1}, write: privatefile.Write}
	if root == "" {
		return c
	}
	saved, err := read(root)
	if err != nil {
		c.paused = true
		c.loadErr = ErrUnavailable
		return c
	}
	c.state, c.paused = saved, saved.Enabled
	return c
}
func (c *Coordinator) Status() Status {
	if c == nil {
		return Status{State: State{Schema: 1}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{State: c.state, ActiveRequests: c.active, Uncertain: c.uncertain}
	s.Enabled = c.paused
	if c.loadErr != nil {
		s.Error = c.loadErr.Error()
	}
	return s
}
func (c *Coordinator) Enter() (func(), error) {
	return c.enter(false)
}

// Continue permits input/cancel while draining, but closes even those writes
// after an exclusive drain lease has confirmed the maintenance boundary.
func (c *Coordinator) Continue() (func(), error) {
	return c.enter(true)
}

func (c *Coordinator) enter(continuation bool) (func(), error) {
	if c == nil {
		return func() {}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused && (!continuation || c.drainRelease != nil) {
		return nil, ErrPaused
	}
	c.active++
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); c.active--; c.mu.Unlock() }) }, nil
}
func (c *Coordinator) Change(ctx context.Context, enabled bool, revision int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loadErr != nil || c.root == "" {
		return ErrUnavailable
	}
	if revision != c.state.Revision || revision < 0 || revision == math.MaxInt64 {
		return ErrConflict
	}
	// Closing admission precedes the write. A failed exit never reopens it.
	if enabled {
		c.paused = true
	}
	next := State{Schema: 1, Revision: revision + 1, Enabled: enabled}
	data, _ := json.Marshal(next)
	err := c.write(ctx, filepath.Join(c.root, "maintenance.json"), data)
	if errors.Is(err, privatefile.ErrDurabilityUncertain) {
		c.state = next
		c.paused = true
		c.uncertain = true
		return ErrUncertain
	}
	if err != nil {
		return ErrUnavailable
	}
	checked, err := read(c.root)
	if err != nil || checked != next {
		c.state = next
		c.paused = true
		c.uncertain = true
		return ErrUncertain
	}
	c.state, c.paused, c.uncertain = next, enabled, false
	if !enabled && c.drainRelease != nil {
		err = c.drainRelease()
		c.drainRelease = nil
	}
	if err != nil {
		c.paused = true
		return ErrUnavailable
	}
	return nil
}

// HoldDrained is called only after all local producers are paused and their
// activity counts reach zero. The exclusive lease also excludes local CLI
// tasks until maintenance exits. A false result never means safe to back up.
func (c *Coordinator) HoldDrained(localIdle bool) (bool, error) {
	if c == nil {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused || c.uncertain || c.loadErr != nil || c.active != 0 || !localIdle {
		return false, nil
	}
	if c.drainRelease != nil {
		return true, nil
	}
	release, err := workLease(c.root, false)
	if errors.Is(err, accountlock.ErrBusy) {
		return false, nil
	}
	if err != nil {
		return false, ErrUnavailable
	}
	c.drainRelease = release
	return true, nil
}
func (c *Coordinator) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drainRelease == nil {
		return nil
	}
	err := c.drainRelease()
	c.drainRelease = nil
	return err
}

// AcquireTask shares the maintenance work lease and checks the durable mode
// while holding it, closing the race between an idle check and a CLI start.
func AcquireTask(root string) (func() error, error) {
	release, err := workLease(root, true)
	if err != nil {
		return nil, ErrPaused
	}
	saved, err := read(root)
	if err != nil || saved.Enabled {
		_ = release()
		if err != nil {
			return nil, ErrUnavailable
		}
		return nil, ErrPaused
	}
	return release, nil
}
func AcquireInstance(root string) (func() error, error) {
	key, err := rootIdentity(root)
	if err != nil {
		return nil, err
	}
	release, err := accountlock.Acquire("https://console-instance.invalid", key)
	if err != nil {
		return nil, errors.New("CONSOLE_INSTANCE_BUSY: 同一数据目录已有控制台实例，或实例锁不可用")
	}
	return release, nil
}

// AcquireOffline excludes both a console and every CLI writer for an explicit
// offline maintenance command. Durable maintenance may remain enabled: the
// operator need not reopen online admission merely to repair local storage.
func AcquireOffline(root string) (func() error, error) {
	instance, err := AcquireInstance(root)
	if err != nil {
		return nil, err
	}
	work, err := workLease(root, false)
	if err != nil {
		_ = instance()
		return nil, errors.New("OFFLINE_MAINTENANCE_BUSY: 离线操作要求目标控制台和 CLI 均已停止")
	}
	return func() error { return errors.Join(work(), instance()) }, nil
}
func rootIdentity(root string) (string, error) {
	key, err := accountlock.DirectoryIdentity(root)
	if err != nil {
		return "", ErrUnavailable
	}
	return key, nil
}
func workLease(root string, shared bool) (func() error, error) {
	key, err := rootIdentity(root)
	if err != nil {
		return nil, err
	}
	if shared {
		return accountlock.AcquireShared("https://console-maintenance.invalid", key)
	}
	return accountlock.Acquire("https://console-maintenance.invalid", key)
}
func read(root string) (State, error) {
	path := filepath.Join(root, "maintenance.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Schema: 1}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return State{}, ErrUnavailable
	}
	f, err := os.Open(path)
	if err != nil {
		return State{}, ErrUnavailable
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || privatefile.Verify(f) != nil {
		return State{}, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 || authmaterial.ValidateUnambiguousJSON(data) != nil {
		return State{}, ErrUnavailable
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 3 || fields["schema"] == nil || fields["revision"] == nil || fields["enabled"] == nil {
		return State{}, ErrUnavailable
	}
	var decoded struct {
		Schema   *int   `json:"schema"`
		Revision *int64 `json:"revision"`
		Enabled  *bool  `json:"enabled"`
	}
	if json.Unmarshal(data, &decoded) != nil || decoded.Schema == nil || decoded.Revision == nil || decoded.Enabled == nil || *decoded.Schema != 1 || *decoded.Revision < 1 {
		return State{}, ErrUnavailable
	}
	saved := State{Schema: *decoded.Schema, Revision: *decoded.Revision, Enabled: *decoded.Enabled}
	if saved.Schema != 1 || saved.Revision < 1 {
		return State{}, ErrUnavailable
	}
	// A previous exit may have replaced the file before a failed directory
	// flush. Resolve that purely local uncertainty before admitting new work.
	if !saved.Enabled && privatefile.SyncDirectory(root) != nil {
		return State{}, ErrUnavailable
	}
	return saved, nil
}
