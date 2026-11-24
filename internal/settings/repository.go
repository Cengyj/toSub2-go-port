// Package settings stores private console configuration independently of
// account credentials and operation recovery evidence. Saving a preference
// never starts a login, provider request, monitor or upload.
package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/privatestate"
)

type Scope string

const (
	SMS            Scope = "sms"
	Mail           Scope = "mail"
	Sub2API        Scope = "sub2api"
	Proxy          Scope = "proxy"
	maxConfigBytes       = 256 << 10
	purpose              = "console-settings-v1"
)

var (
	ErrInvalid     = errors.New("SETTINGS_INVALID: 配置范围、版本或内容无效")
	ErrConflict    = errors.New("SETTINGS_CONFLICT: 服务端配置已变化，请明确重新读取后再保存；浏览器配置仍保留")
	ErrUnavailable = errors.New("SETTINGS_UNAVAILABLE: 受保护的配置存储不可用，未清除浏览器配置")
	ErrUncertain   = errors.New("SETTINGS_UNCERTAIN: 配置保存结果待核实，未清除浏览器配置；请先读取并确认保存结果")
)

// Snapshot.Config is private. It may only be returned by authenticated,
// no-store endpoints, and must never enter ordinary logs or public projections.
type Snapshot struct {
	Version  int             `json:"version"`
	Scope    Scope           `json:"scope"`
	Revision int64           `json:"revision"`
	Config   json.RawMessage `json:"config"`
}

type Repository struct {
	root      string
	registry  *privatestate.Registry
	available bool
	read      func(context.Context, string, string, int64) ([]byte, privatestate.Binding, error)
	write     func(context.Context, string, string, string, string, any) error
	syncDir   func(string) error
}

func New(root string, protector credentials.StateProtector) *Repository {
	r := &Repository{registry: privatestate.NewRegistry(), syncDir: privatefile.SyncDirectory}
	absolute, err := filepath.Abs(root)
	if root == "" || err != nil {
		return r
	}
	r.root = absolute
	if r.registry.Require(absolute) != nil {
		return r
	}
	r.available = r.registry.Register(absolute, protector) == nil
	r.read, r.write = r.registry.ReadJSONWithBinding, r.registry.WriteJSON
	return r
}

func (r *Repository) Available() bool { return r != nil && r.available }

func validScope(scope Scope) bool {
	switch scope {
	case SMS, Mail, Sub2API, Proxy:
		return true
	}
	return false
}
func (r *Repository) path(scope Scope) string {
	return filepath.Join(r.root, "console-settings", string(scope)+".json")
}
func identity(scope Scope) string { return "console-settings/" + string(scope) }

func (r *Repository) Load(ctx context.Context, scope Scope) (Snapshot, error) {
	if !validScope(scope) {
		return Snapshot{}, ErrInvalid
	}
	if !r.Available() {
		return Snapshot{}, ErrUnavailable
	}
	return r.load(ctx, scope)
}

// HoldRevision binds an explicit process application to the saved generation.
// The caller keeps this scope's same OS lease used by Save/Confirm until its
// local configuration update finishes; checking then releasing before applying
// would allow another browser or process to publish a newer generation.
func (r *Repository) HoldRevision(ctx context.Context, scope Scope, revision int64) (func() error, error) {
	if !validScope(scope) || revision < 1 {
		return nil, ErrInvalid
	}
	if !r.Available() {
		return nil, ErrUnavailable
	}
	release, err := r.acquire(scope)
	if err != nil {
		return nil, err
	}
	current, err := r.load(ctx, scope)
	if err == nil && current.Revision != revision {
		err = ErrConflict
	}
	if err != nil {
		_ = release()
		return nil, err
	}
	return release, nil
}

func (r *Repository) load(ctx context.Context, scope Scope) (Snapshot, error) {
	raw, binding, err := r.read(ctx, r.path(scope), purpose, maxConfigBytes+4096)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{Version: 1, Scope: scope, Config: json.RawMessage("null")}, nil
	}
	if err != nil {
		return Snapshot{}, ErrUnavailable
	}
	var saved Snapshot
	if json.Unmarshal(raw, &saved) != nil || saved.Version != 1 || saved.Scope != scope || saved.Revision < 1 ||
		binding.Schema != privatestate.Schema || binding.Identity != privatestate.Identity(identity(scope)) || binding.Operation != strconv.FormatInt(saved.Revision, 10) || validateConfig(saved.Config) != nil {
		return Snapshot{}, ErrUnavailable
	}
	return saved, nil
}

func validateConfig(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > maxConfigBytes || raw[0] != '{' || authmaterial.ValidateUnambiguousJSON(raw) != nil {
		return ErrInvalid
	}
	return nil
}

// Save uses a per-root, per-scope OS lease and reads the current revision while
// holding it. Two browser tabs or two local processes cannot silently replace
// each other's configuration. Unknown fields and explicit zero values survive.
func (r *Repository) Save(ctx context.Context, scope Scope, revision int64, config json.RawMessage) (Snapshot, error) {
	if !validScope(scope) || revision < 0 || revision == math.MaxInt64 || validateConfig(config) != nil {
		return Snapshot{}, ErrInvalid
	}
	if !r.Available() {
		return Snapshot{}, ErrUnavailable
	}
	release, err := r.acquire(scope)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	current, err := r.load(ctx, scope)
	if err != nil {
		return Snapshot{}, err
	}
	if current.Revision != revision {
		return Snapshot{}, ErrConflict
	}
	next := Snapshot{Version: 1, Scope: scope, Revision: revision + 1, Config: append(json.RawMessage(nil), config...)}
	if err := r.write(ctx, r.path(scope), purpose, identity(scope), strconv.FormatInt(next.Revision, 10), next); err != nil {
		if errors.Is(err, privatefile.ErrDurabilityUncertain) {
			return next, ErrUncertain
		}
		return Snapshot{}, ErrUnavailable
	}
	// A write receipt alone is not sufficient to remove the browser's only
	// copy. Require decryption and complete configuration readback first.
	readback, err := r.load(ctx, scope)
	if err != nil || !equal(next, readback) {
		return next, ErrUncertain
	}
	return readback, nil
}

// Confirm only completes local durability/readback after an unknown save. It
// never resends an external request or replaces the configuration.
func (r *Repository) Confirm(ctx context.Context, scope Scope, revision int64) (Snapshot, error) {
	if !validScope(scope) || revision < 1 {
		return Snapshot{}, ErrInvalid
	}
	if !r.Available() {
		return Snapshot{}, ErrUnavailable
	}
	release, err := r.acquire(scope)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	saved, err := r.load(ctx, scope)
	if err != nil {
		return Snapshot{}, err
	}
	if saved.Revision != revision {
		return Snapshot{}, ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := r.syncDir(filepath.Dir(r.path(scope))); err != nil {
		return saved, ErrUncertain
	}
	checked, err := r.load(ctx, scope)
	if err != nil || !equal(saved, checked) {
		return saved, ErrUncertain
	}
	return checked, nil
}

func equal(a, b Snapshot) bool {
	var left, right bytes.Buffer
	return a.Version == b.Version && a.Scope == b.Scope && a.Revision == b.Revision &&
		json.Compact(&left, a.Config) == nil && json.Compact(&right, b.Config) == nil && bytes.Equal(left.Bytes(), right.Bytes())
}

func (r *Repository) acquire(scope Scope) (func() error, error) {
	rootIdentity, err := accountlock.DirectoryIdentity(r.root)
	if err != nil {
		return nil, ErrUnavailable
	}
	// Resolve aliases only for coordination. The original root still governs
	// protected reads/writes, including rejection of a symlink at that root.
	key := rootIdentity + "/" + string(scope)
	release, err := accountlock.Acquire("https://console-settings.invalid", key)
	if errors.Is(err, accountlock.ErrBusy) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	return release, nil
}
