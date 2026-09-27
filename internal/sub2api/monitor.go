package sub2api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RemoteAccount contains only the fields needed by auto-repair. Credentials
// and extra are kept in memory for the caller's update operation and are never
// serialized in MonitorState or returned in summaries.
type RemoteAccount struct {
	ID          int64
	Email       string
	GroupIDs    []int
	Status      string
	Credentials map[string]any
	Extra       map[string]any
}

func (a RemoteAccount) Public() map[string]any {
	return map[string]any{"id": a.ID, "email": a.Email, "groupIds": append([]int(nil), a.GroupIDs...), "status": a.Status}
}

// DecodeRemoteAccounts converts the untrusted API response into a minimal
// in-memory representation. It deliberately does not retain arbitrary fields.
func DecodeRemoteAccounts(value any) []RemoteAccount {
	items := []any{}
	switch x := value.(type) {
	case []any:
		items = x
	case map[string]any:
		if d, ok := x["data"]; ok {
			return DecodeRemoteAccounts(d)
		}
		if d, ok := x["items"].([]any); ok {
			items = d
		} else if _, ok := x["id"]; ok {
			items = []any{x}
		}
	}
	out := make([]RemoteAccount, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		a := RemoteAccount{Status: stringValue(m["status"]), Email: AccountEmail(m)}
		a.ID = intValue(m["id"])
		a.GroupIDs = readGroupIDs(m)
		if c, ok := m["credentials"].(map[string]any); ok {
			a.Credentials = c
		}
		if e, ok := m["extra"].(map[string]any); ok {
			a.Extra = e
		}
		if a.ID > 0 {
			out = append(out, a)
		}
	}
	return out
}
func readGroupIDs(m map[string]any) []int {
	var out []int
	if xs, ok := m["group_ids"].([]any); ok {
		for _, x := range xs {
			if n := intValue(x); n > 0 {
				out = append(out, int(n))
			}
		}
	}
	if xs, ok := m["account_groups"].([]any); ok {
		for _, x := range xs {
			if g, ok := x.(map[string]any); ok {
				if n := intValue(g["group_id"]); n > 0 {
					out = append(out, int(n))
				}
			}
		}
	}
	return out
}
func intValue(v any) int64 {
	// The JSON format is shared with JavaScript. Reject fractions and values
	// beyond its exact integer range instead of silently targeting another ID.
	const maxSafeInteger = int64(1<<53 - 1)
	var n int64
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || math.Trunc(x) != x || x < 0 || x > float64(maxSafeInteger) {
			return 0
		}
		n = int64(x)
	case int:
		n = int64(x)
	case int64:
		n = x
	case json.Number:
		n, _ = x.Int64()
	case string:
		n, _ = strconv.ParseInt(strings.TrimSpace(x), 10, 64)
	}
	if n < 0 || n > maxSafeInteger {
		return 0
	}
	return n
}
func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

type RepairAction string

const (
	ActionMissing    RepairAction = "missing_task"
	ActionBlocked    RepairAction = "blocked"
	ActionBusy       RepairAction = "busy"
	ActionCooldown   RepairAction = "cooldown"
	ActionIneligible RepairAction = "ineligible"
	ActionStart      RepairAction = "start"
)

type RepairDecision struct {
	Action RepairAction
	Reason string
	// ClearPermanent is explicit evidence from the job layer that a later
	// successful authorization cleared its permanent failure state. Busy or
	// temporarily ineligible decisions alone never imply that evidence.
	ClearPermanent bool
}

// MonitorHooks adapts the monitor to jobs without importing the jobs package.
// StartRepair receives all remote records for one email, allowing duplicate
// remote IDs to be updated in one authorization operation.
type MonitorHooks struct {
	Evaluate     func(context.Context, string, []RemoteAccount) (RepairDecision, error)
	StartRepair  func(context.Context, string, []RemoteAccount) error
	RetryPending func(context.Context) (int, error)
}

type MonitorConfig struct {
	CodexFingerprintMode string
	BaseURL              string
	GroupIDs             []int
	Enabled              bool
	Interval             time.Duration
	Cooldown             time.Duration
	// CooldownConfigured distinguishes an explicit zero (disabled) from defaults.
	CooldownConfigured bool
}
type MonitorSummary struct {
	Trigger       string `json:"trigger"`
	Checked       int    `json:"checked"`
	Matched       int    `json:"matched"`
	Started       int    `json:"started"`
	Updated       int    `json:"updated"`
	MissingTask   int    `json:"missingTask"`
	Ineligible    int    `json:"ineligible"`
	Blocked       int    `json:"blocked"`
	Busy          int    `json:"busy"`
	Cooldown      int    `json:"cooldown"`
	OutsideGroups int    `json:"outsideGroups"`
	MissingEmail  int    `json:"missingEmail"`
	Error         string `json:"error,omitempty"`
}
type MonitorState struct {
	CodexFingerprintMode string                   `json:"codexFingerprintMode,omitempty"`
	Version              int                      `json:"version"`
	Enabled              bool                     `json:"enabled"`
	BaseURL              string                   `json:"baseUrl,omitempty"`
	GroupIDs             []int                    `json:"groupIds,omitempty"`
	LastCheckAt          *time.Time               `json:"lastCheckAt,omitempty"`
	LastError            string                   `json:"lastError,omitempty"`
	LastResult           *MonitorSummary          `json:"lastResult,omitempty"`
	Cooldowns            map[string]CooldownEntry `json:"cooldowns,omitempty"`
	Permanent            map[string]string        `json:"permanent,omitempty"`
	UpdatedAt            time.Time                `json:"updatedAt"`
}
type CooldownEntry struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}
type MonitorService struct {
	Client       *Client
	Config       MonitorConfig
	Hooks        MonitorHooks
	StatePath    string
	mu           sync.Mutex
	State        MonitorState
	running      bool
	legacyAPIKey string
	saveMu       sync.Mutex
}

func normalizeMonitorDurations(cfg MonitorConfig) MonitorConfig {
	if cfg.Interval < time.Second {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Cooldown < 0 || (cfg.Cooldown == 0 && !cfg.CooldownConfigured) {
		cfg.Cooldown = 5 * time.Minute
	}
	cfg.CooldownConfigured = true
	return cfg
}

func NewMonitorService(client *Client, cfg MonitorConfig, hooks MonitorHooks, statePath string) *MonitorService {
	cfg = normalizeMonitorDurations(cfg)
	if cfg.BaseURL == "" && client != nil {
		cfg.BaseURL = client.cfg.BaseURL
	}
	s := &MonitorService{Client: client, Config: cfg, Hooks: hooks, StatePath: statePath, State: MonitorState{Version: 1, Enabled: cfg.Enabled, BaseURL: cfg.BaseURL, GroupIDs: append([]int(nil), cfg.GroupIDs...), Cooldowns: map[string]CooldownEntry{}, Permanent: map[string]string{}}}
	if e := s.Load(); e != nil {
		s.State.LastError = "monitor state could not be loaded"
	}
	return s
}

func (s *MonitorService) Load() error {
	if s.StatePath == "" {
		return nil
	}
	b, e := os.ReadFile(s.StatePath)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var st MonitorState
	if e = json.Unmarshal(b, &st); e != nil {
		return e
	}
	var legacy struct {
		Config *struct {
			BaseURL              string `json:"baseUrl"`
			CodexFingerprintMode string `json:"codexFingerprintMode"`
			AdminAPIKey          string `json:"adminApiKey"`
			APIKey               string `json:"apiKey"`
			GroupIDs             []int  `json:"groupIds"`
		} `json:"config"`
		State *struct {
			LastCheckAt *time.Time      `json:"lastCheckAt"`
			LastError   string          `json:"lastError"`
			LastResult  *MonitorSummary `json:"lastResult"`
		} `json:"state"`
	}
	if err := json.Unmarshal(b, &legacy); err != nil {
		return err
	}
	legacyKey := ""
	if legacy.Config != nil {
		st.BaseURL = legacy.Config.BaseURL
		st.GroupIDs = legacy.Config.GroupIDs
		st.CodexFingerprintMode = legacy.Config.CodexFingerprintMode
		legacyKey = legacy.Config.AdminAPIKey
		if legacyKey == "" {
			legacyKey = legacy.Config.APIKey
		}
	}
	if legacy.State != nil {
		st.LastCheckAt = legacy.State.LastCheckAt
		st.LastError = legacy.State.LastError
		st.LastResult = legacy.State.LastResult
	}

	if st.Version == 0 {
		st.Version = 1
	}
	if st.BaseURL != "" {
		base, err := normalizeBaseURL(st.BaseURL)
		if err != nil {
			return errors.New("invalid persisted Sub2API URL")
		}
		st.BaseURL = base
	}
	if st.LastError != "" {
		st.LastError = "Previous Sub2API monitor operation failed"
	}
	if st.LastResult != nil && st.LastResult.Error != "" {
		st.LastResult.Error = "Previous Sub2API monitor operation failed"
	}
	for email, cd := range st.Cooldowns {
		if cd.Reason != "" {
			cd.Reason = "previous repair attempt"
		}
		st.Cooldowns[email] = cd
	}
	for email := range st.Permanent {
		st.Permanent[email] = "account permanently unavailable"
	}

	if st.Cooldowns == nil {
		st.Cooldowns = map[string]CooldownEntry{}
	}
	if st.Permanent == nil {
		st.Permanent = map[string]string{}
	}
	s.mu.Lock()
	s.State = st
	if s.Config.Cooldown == 0 {
		s.State.Cooldowns = map[string]CooldownEntry{}
	}
	s.legacyAPIKey = legacyKey
	if s.Config.BaseURL == "" {
		s.Config.BaseURL = st.BaseURL
		s.Config.CodexFingerprintMode = st.CodexFingerprintMode
		s.Config.Enabled = st.Enabled
		s.Config.GroupIDs = append([]int(nil), st.GroupIDs...)
	}
	s.mu.Unlock()
	return nil
}
func (s *MonitorService) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if s.StatePath == "" {
		return nil
	}
	s.mu.Lock()
	st := s.cloneStateLocked()
	if st.Version == 0 {
		st.Version = 1
	}
	st.BaseURL = s.Config.BaseURL
	st.CodexFingerprintMode = s.Config.CodexFingerprintMode
	st.Enabled = s.Config.Enabled
	st.GroupIDs = append([]int(nil), s.Config.GroupIDs...)
	st.UpdatedAt = time.Now().UTC()
	s.State.UpdatedAt = st.UpdatedAt
	s.mu.Unlock()
	b, e := json.MarshalIndent(st, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	if e = os.MkdirAll(filepath.Dir(s.StatePath), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(s.StatePath), ".monitor-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(name, s.StatePath)
}

func (s *MonitorService) Check(ctx context.Context, trigger string) (MonitorSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return MonitorSummary{}, errors.New("Sub2API monitor check already running")
	}
	if !s.Config.Enabled {
		s.mu.Unlock()
		return MonitorSummary{}, errors.New("Sub2API monitor is disabled")
	}
	s.running = true
	client, cfg, hooks := s.Client, s.Config, s.Hooks
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()
	sum := MonitorSummary{Trigger: trigger}
	if client == nil {
		err := errors.New("Sub2API monitor client is not configured")
		sum.Error = "Sub2API monitor credentials are unavailable"
		s.recordResult(sum, err)
		return sum, err
	}
	if hooks.RetryPending != nil {
		n, e := hooks.RetryPending(ctx)
		sum.Updated += n
		if e != nil {
			sum.Error = SafeError(e)
		}
	}
	accounts, e := client.ErrorAccounts(ctx, nil)
	if e != nil {
		sum.Error = SafeError(e)
		s.recordResult(sum, e)
		return sum, e
	}
	sum.Checked = len(accounts)
	grouped := map[string][]RemoteAccount{}
	for _, raw := range accounts {
		decoded := DecodeRemoteAccounts([]any{raw})
		if len(decoded) == 0 {
			continue
		}
		a := decoded[0]
		if len(cfg.GroupIDs) > 0 && !groupsContain(a.GroupIDs, cfg.GroupIDs) {
			sum.OutsideGroups++
			continue
		}
		if a.Email == "" {
			sum.MissingEmail++
			continue
		}
		k := strings.ToLower(a.Email)
		grouped[k] = append(grouped[k], a)
	}
	emails := make([]string, 0, len(grouped))
	for email := range grouped {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	for _, email := range emails {
		if err := ctx.Err(); err != nil {
			sum.Error = SafeError(err)
			break
		}
		as := grouped[email]
		decision := s.decision(ctx, email, as)
		if decision.Action != ActionMissing {
			sum.Matched += len(as)
		}
		switch decision.Action {
		case ActionMissing:
			sum.MissingTask += len(as)
		case ActionBlocked:
			sum.Blocked += len(as)
			s.markPermanent(email, decision.Reason)
		case ActionBusy:
			sum.Busy += len(as)
		case ActionCooldown:
			sum.Cooldown += len(as)
		case ActionIneligible:
			sum.Ineligible += len(as)
		case ActionStart:
			if hooks.StartRepair == nil {
				sum.Ineligible += len(as)
				continue
			}
			if e := hooks.StartRepair(ctx, email, as); e != nil {
				sum.Error = SafeError(e)
				s.markCooldown(email, sum.Error)
			} else {
				sum.Started += len(as)
				s.markCooldown(email, "repair started")
			}
		}
	}
	var resultErr error
	if err := ctx.Err(); err != nil {
		resultErr = err
		sum.Error = SafeError(err)
	} else if sum.Error != "" {
		resultErr = errors.New("Sub2API monitor check failed")
	}
	if err := s.recordResult(sum, resultErr); err != nil {
		return sum, err
	}
	return sum, resultErr
}
func (s *MonitorService) decision(ctx context.Context, email string, a []RemoteAccount) RepairDecision {
	s.mu.Lock()
	reason, permanent := s.State.Permanent[email]
	cd, hasCooldown := s.State.Cooldowns[email]
	hasCooldown = hasCooldown && s.Config.Cooldown > 0
	evaluate := s.Hooks.Evaluate
	s.mu.Unlock()
	if permanent {
		blocked := RepairDecision{Action: ActionBlocked, Reason: reason}
		if evaluate == nil {
			return blocked
		}
		d, err := evaluate(ctx, email, a)
		if err != nil || !d.ClearPermanent {
			return blocked
		}
		switch d.Action {
		case ActionStart, ActionBusy, ActionCooldown, ActionIneligible:
			s.mu.Lock()
			delete(s.State.Permanent, email)
			s.mu.Unlock()
			if err := s.Save(); err != nil {
				return RepairDecision{Action: ActionIneligible, Reason: "monitor state could not be saved"}
			}
		default:
			return blocked
		}
		if d.Action == ActionStart && hasCooldown && time.Now().Before(cd.Until) {
			return RepairDecision{Action: ActionCooldown, Reason: cd.Reason}
		}
		return d
	}
	if hasCooldown && time.Now().Before(cd.Until) {
		return RepairDecision{Action: ActionCooldown, Reason: cd.Reason}
	}
	if evaluate == nil {
		return RepairDecision{Action: ActionIneligible, Reason: "monitor hooks not configured"}
	}
	d, e := evaluate(ctx, email, a)
	if e != nil {
		return RepairDecision{Action: ActionIneligible, Reason: SafeError(e)}
	}
	return d
}
func (s *MonitorService) recordResult(sum MonitorSummary, e error) error {
	now := time.Now().UTC()
	s.mu.Lock()
	s.State.LastCheckAt = &now
	s.State.LastResult = &sum
	if e != nil {
		s.State.LastError = SafeError(e)
	} else {
		s.State.LastError = ""
	}
	s.mu.Unlock()
	if err := s.Save(); err != nil {
		s.mu.Lock()
		s.State.LastError = "Monitor state could not be saved"
		s.mu.Unlock()
		return errors.New("monitor state could not be saved")
	}
	return nil
}
func (s *MonitorService) markCooldown(email, reason string) {
	s.mu.Lock()
	if s.State.Cooldowns == nil {
		s.State.Cooldowns = map[string]CooldownEntry{}
	}
	if s.Config.Cooldown > 0 {
		s.State.Cooldowns[email] = CooldownEntry{Until: time.Now().Add(s.Config.Cooldown), Reason: reason}
	} else {
		delete(s.State.Cooldowns, email)
	}
	s.mu.Unlock()
	_ = s.Save()
}
func (s *MonitorService) markPermanent(email, reason string) {
	s.mu.Lock()
	if s.State.Permanent == nil {
		s.State.Permanent = map[string]string{}
	}
	s.State.Permanent[email] = "account permanently unavailable"
	s.mu.Unlock()
	_ = s.Save()
}
func groupsContain(have, want []int) bool {
	for _, w := range want {
		for _, h := range have {
			if h == w {
				return true
			}
		}
	}
	return false
}
func NewIdempotencyKey() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "tosub2-upload"
	}
	return "tosub2-upload-" + hex.EncodeToString(b)
}
func AccountEmailSafe(account map[string]any) string {
	return strings.ToLower(strings.TrimSpace(AccountEmail(account)))
}

var emailRe = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9.-]+\.[a-z]{2,}`)

func GroupRemoteAccounts(accounts []RemoteAccount) map[string][]RemoteAccount {
	out := map[string][]RemoteAccount{}
	for _, a := range accounts {
		if e := emailRe.FindString(strings.ToLower(a.Email)); e != "" {
			out[e] = append(out[e], a)
		}
	}
	for _, v := range out {
		sort.Slice(v, func(i, j int) bool { return v[i].ID < v[j].ID })
	}
	return out
}
func (s *MonitorService) PublicState() MonitorState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.cloneStateLocked()
	st.Enabled = s.Config.Enabled
	st.BaseURL = s.Config.BaseURL
	st.CodexFingerprintMode = s.Config.CodexFingerprintMode
	st.GroupIDs = append([]int(nil), s.Config.GroupIDs...)
	st.Cooldowns = nil
	st.Permanent = nil
	return st
}

func (s *MonitorService) String() string {
	st := s.PublicState()
	return fmt.Sprintf("sub2api monitor enabled=%t last=%v", st.Enabled, st.LastCheckAt)
}

// Configure validates and atomically replaces one monitor configuration. API
// keys live only in Client; the persisted state contains no credential material.
func (s *MonitorService) Configure(client *Client, cfg MonitorConfig, hooks MonitorHooks) error {
	if cfg.BaseURL != "" {
		base, err := normalizeBaseURL(cfg.BaseURL)
		if err != nil {
			return err
		}
		cfg.BaseURL = base
	}
	if cfg.Enabled && (client == nil || client.cfg.APIKey == "" || cfg.BaseURL == "") {
		return errors.New("Sub2API URL and API key are required")
	}
	cfg = normalizeMonitorDurations(cfg)
	cfg.GroupIDs = append([]int(nil), cfg.GroupIDs...)
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("Sub2API monitor check already running")
	}
	if s.Config.BaseURL != cfg.BaseURL {
		s.State.Cooldowns = map[string]CooldownEntry{}
		s.State.Permanent = map[string]string{}
	}
	if cfg.Cooldown == 0 {
		s.State.Cooldowns = map[string]CooldownEntry{}
	}
	if cfg.CodexFingerprintMode == "" {
		cfg.CodexFingerprintMode = "session"
	}
	s.State.CodexFingerprintMode = cfg.CodexFingerprintMode
	s.Config, s.Client, s.Hooks = cfg, client, hooks
	s.State.Enabled, s.State.BaseURL, s.State.GroupIDs = cfg.Enabled, cfg.BaseURL, append([]int(nil), cfg.GroupIDs...)
	s.mu.Unlock()
	return s.Save()
}
func (s *MonitorService) Configuration() MonitorConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.Config
	c.GroupIDs = append([]int(nil), c.GroupIDs...)
	return c
}
func (s *MonitorService) Running() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.running }
func (s *MonitorService) cloneStateLocked() MonitorState {
	st := s.State
	st.GroupIDs = append([]int(nil), st.GroupIDs...)
	st.Cooldowns = make(map[string]CooldownEntry, len(s.State.Cooldowns))
	for k, v := range s.State.Cooldowns {
		st.Cooldowns[k] = v
	}
	st.Permanent = make(map[string]string, len(s.State.Permanent))
	for k, v := range s.State.Permanent {
		st.Permanent[k] = v
	}
	if st.LastResult != nil {
		v := *st.LastResult
		st.LastResult = &v
	}
	if st.LastCheckAt != nil {
		v := *st.LastCheckAt
		st.LastCheckAt = &v
	}
	return st
}

// BackendIdentity matches the legacy pending-repair backend identifier.
func BackendIdentity(base string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(strings.TrimSpace(base), "/")))
	return hex.EncodeToString(sum[:])[:24]
}

// SyncAccountCredentials completes one remote update. A missing remote record
// is terminal for that ID. Local OAuth fields augment, rather than erase, the
// remote provider-specific credential fields.
var ErrAccountMissing = errors.New("Sub2API remote account no longer exists")

func (c *Client) SyncAccountCredentials(ctx context.Context, id int, local map[string]any, mode string) error {
	return c.SyncAccountCredentialsGuarded(ctx, id, local, mode, nil)
}

// SyncAccountCredentialsGuarded checks whether the authorization is still
// current before each remote lifecycle step. A request already accepted by the
// server cannot be undone; the guard prevents later steps from continuing when
// a user has started a newer operation or permanently disabled the account.
func (c *Client) SyncAccountCredentialsGuarded(ctx context.Context, id int, local map[string]any, mode string, guard func() error) error {
	if id <= 0 || intValue(id) == 0 {
		return errors.New("invalid Sub2API account ID")
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if guard != nil {
			return guard()
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	sid := fmt.Sprint(id)
	remote, err := c.Account(ctx, sid)
	if IsRemoteStatus(err, 404) {
		return ErrAccountMissing
	}
	if err != nil {
		return err
	}
	rm, _ := remote.(map[string]any)
	if data, ok := rm["data"].(map[string]any); ok {
		rm = data
	}
	if rm == nil || intValue(rm["id"]) != int64(id) {
		return errors.New("Sub2API account response is invalid")
	}
	if localEmail := stringValue(local["email"]); localEmail != "" {
		if remoteEmail := AccountEmail(rm); remoteEmail != "" && !strings.EqualFold(localEmail, remoteEmail) {
			return errors.New("Sub2API account email changed; refusing unrelated credential update")
		}
	}
	merged := map[string]any{}
	if credentials, ok := rm["credentials"].(map[string]any); ok {
		for k, v := range credentials {
			merged[k] = v
		}
	}
	for k, v := range local {
		merged[k] = v
	}
	extra := map[string]any{}
	if existing, ok := rm["extra"].(map[string]any); ok {
		for k, v := range existing {
			extra[k] = v
		}
	}
	if mode != "" {
		extra["codex_fingerprint_mode"] = mode
	}
	if err := check(); err != nil {
		return err
	}
	if _, err = c.Update(ctx, sid, map[string]any{"credentials": merged, "extra": extra}); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err = c.ClearError(ctx, sid); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	return c.SetSchedulable(ctx, sid, true)
}

// TakeLegacyAPIKey transfers a legacy plaintext configuration key exactly once
// to the caller responsible for OS credential migration. Save never writes it.
func (s *MonitorService) TakeLegacyAPIKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.legacyAPIKey
	s.legacyAPIKey = ""
	return key
}
func (s *MonitorService) SetOperationalError(message string) {
	s.mu.Lock()
	s.State.LastError = message
	s.mu.Unlock()
	_ = s.Save()
}
