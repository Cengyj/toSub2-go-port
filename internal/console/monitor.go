package console

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

func (s *Server) monitorFile() string {
	if s.cfg.OutputRoot == "" {
		return ""
	}
	return filepath.Join(s.cfg.OutputRoot, "sub2api-monitor.json")
}
func monitorCredentialID(base string) string {
	return "sub2api-monitor-" + sub2api.BackendIdentity(base)
}

func (s *Server) initializeMonitor() {
	s.monitorWake = make(chan struct{}, 1)
	if s.cfg.OutputRoot != "" && s.monitorVault == nil {
		if vault, err := credentials.NewSecureFileStore(filepath.Join(s.cfg.OutputRoot, "credentials")); err == nil {
			s.monitorVault = vault
		}
	}
	s.monitor = sub2api.NewMonitorService(nil, sub2api.MonitorConfig{
		Interval:           monitorDuration("SUB2API_MONITOR_INTERVAL_MS", 5*time.Minute, time.Second),
		Cooldown:           monitorDuration("SUB2API_AUTO_REPAIR_COOLDOWN_MS", 5*time.Minute, 0),
		CooldownConfigured: true,
	}, sub2api.MonitorHooks{}, s.monitorFile())
	saved := s.monitor.Configuration()
	cfg := sub2api.Config{BaseURL: saved.BaseURL, GroupIDs: saved.GroupIDs, ExcludedPlanTypes: saved.ExcludedPlanTypes, CodexFingerprintMode: saved.CodexFingerprintMode, OpenAIRequestTimezone: saved.OpenAIRequestTimezone}
	enabled := saved.Enabled
	_, err := os.Stat(s.monitorFile())
	if os.IsNotExist(err) || s.monitorFile() == "" {
		cfg.BaseURL = strings.TrimSpace(os.Getenv("SUB2API_BASE_URL"))
		enabled = cfg.BaseURL != "" && strings.TrimSpace(os.Getenv("SUB2API_ADMIN_API_KEY")) != ""
	}
	if cfg.BaseURL != "" && s.monitorVault != nil {
		if secret, err := s.smsVaultLoad(monitorCredentialID(cfg.BaseURL)); err == nil {
			cfg.APIKey = secret.Password
			s.monitorKeyPersistent = true
		}
	}
	if key := strings.TrimSpace(os.Getenv("SUB2API_ADMIN_API_KEY")); key != "" {
		cfg.APIKey = key
		s.monitorKeyPersistent = true
	}
	legacyKey := s.monitor.TakeLegacyAPIKey()
	migrationWarning := ""
	if legacyKey != "" {
		if cfg.APIKey == "" {
			cfg.APIKey = legacyKey
		}
		if s.monitorVault != nil {
			if err := s.smsVaultSave(monitorCredentialID(cfg.BaseURL), credentials.Credentials{Password: cfg.APIKey}); err == nil {
				s.monitorKeyPersistent = true
			} else {
				migrationWarning = "旧监控API Key未能保存到系统凭据服务，仅本次运行可用"
			}
		} else {
			migrationWarning = "旧监控API Key已从普通JSON移除，仅本次运行可用；重启需重新配置"
		}
	}
	s.monitorConfig = cfg
	// Persisted enabled state is preserved when credentials are unavailable;
	// checks report the missing key without performing unauthenticated requests.
	if cfg.APIKey == "" {
		return
	}
	mc := saved
	mc.BaseURL, mc.GroupIDs, mc.Enabled = cfg.BaseURL, cfg.GroupIDs, enabled
	mc.OpenAIRequestTimezone = cfg.OpenAIRequestTimezone
	mc.Interval = monitorDuration("SUB2API_MONITOR_INTERVAL_MS", 5*time.Minute, time.Second)
	mc.Cooldown = monitorDuration("SUB2API_AUTO_REPAIR_COOLDOWN_MS", 5*time.Minute, 0)
	mc.CooldownConfigured = true
	if err := s.monitor.Configure(sub2api.New(cfg), mc, s.monitorHooks(cfg)); err != nil && s.logger != nil {
		s.logger.Warn("Sub2API monitor configuration could not be loaded")
	}
	if migrationWarning != "" {
		s.monitor.SetOperationalError(migrationWarning)
	}
}

var monitorDecimalNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Match the legacy readDurationEnv Number(raw) contract: an unset or exactly
// empty value uses the default, whitespace is zero, and invalid/below-minimum
// numbers use the default rather than being clamped to the minimum.
func monitorDuration(name string, fallback, minimum time.Duration) time.Duration {
	raw, exists := os.LookupEnv(name)
	if !exists || raw == "" {
		return fallback
	}
	// ECMAScript whitespace, including BOM, differs from Go's Unicode IsSpace.
	value := strings.Trim(raw, "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff")
	milliseconds := float64(0)
	var err error
	if value != "" {
		if len(value) > 2 && value[0] == '0' && strings.ContainsAny(value[1:2], "xXoObB") {
			base := 16
			switch value[1] {
			case 'o', 'O':
				base = 8
			case 'b', 'B':
				base = 2
			}
			var parsed uint64
			parsed, err = strconv.ParseUint(value[2:], base, 64)
			milliseconds = float64(parsed)
		} else if monitorDecimalNumber.MatchString(value) {
			milliseconds, err = strconv.ParseFloat(value, 64)
		} else {
			return fallback
		}
	}
	nanoseconds := milliseconds * float64(time.Millisecond)
	if err != nil || math.IsNaN(milliseconds) || math.IsInf(milliseconds, 0) || milliseconds < float64(minimum)/float64(time.Millisecond) || nanoseconds >= float64(math.MaxInt64) {
		return fallback
	}
	return time.Duration(nanoseconds)
}

func monitorPublicMinutes(value time.Duration) float64 {
	return math.Max(1, math.Round(value.Minutes()))
}
func (s *Server) configureMonitor(cfg sub2api.Config, enabled, saveKey bool) error {
	return s.configureMonitorContext(context.Background(), cfg, enabled, saveKey)
}

func (s *Server) configureMonitorContext(ctx context.Context, cfg sub2api.Config, enabled, saveKey bool) error {
	cfg.GroupIDs = append([]int(nil), cfg.GroupIDs...)
	cfg.ExcludedPlanTypes = sub2api.NormalizeExcludedPlanTypes(cfg.ExcludedPlanTypes)
	timezone, err := sub2api.NormalizeOpenAIRequestTimezone(cfg.OpenAIRequestTimezone)
	if err != nil {
		return err
	}
	cfg.OpenAIRequestTimezone = timezone
	if cfg.BaseURL != "" {
		base, err := sub2api.ValidateBaseURL(cfg.BaseURL)
		if err != nil {
			return err
		}
		cfg.BaseURL = base
	}
	if enabled && (cfg.BaseURL == "" || cfg.APIKey == "") {
		return errors.New("Sub2API URL and API key are required")
	}
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	if s.monitor.Running() {
		return errors.New("Sub2API monitor check already running")
	}
	persistent := s.monitorKeyPersistent && cfg.BaseURL == s.monitorConfig.BaseURL && cfg.APIKey == s.monitorConfig.APIKey
	if saveKey && cfg.APIKey != "" && s.monitorVault != nil {
		credentialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := credentials.SaveContext(credentialCtx, s.monitorVault, monitorCredentialID(cfg.BaseURL), credentials.Credentials{Password: cfg.APIKey}); err != nil {
			return errors.New("Sub2API API key could not be saved by the secure credential service")
		}
		persistent = true
	}
	mc := s.monitor.Configuration()
	mc.CodexFingerprintMode = cfg.CodexFingerprintMode
	mc.OpenAIRequestTimezone = cfg.OpenAIRequestTimezone
	mc.Enabled, mc.BaseURL, mc.GroupIDs = enabled, cfg.BaseURL, cfg.GroupIDs
	mc.ExcludedPlanTypes = cfg.ExcludedPlanTypes
	if err := s.monitor.Configure(sub2api.New(cfg), mc, s.monitorHooks(cfg)); err != nil {
		return err
	}
	s.monitorConfig, s.monitorKeyPersistent = cfg, persistent
	// Configuration wakeups replace the current schedule. Publish its new
	// deadline only after the scheduler has armed the replacement timer.
	s.monitorNextCheckAt = nil
	select {
	case s.monitorWake <- struct{}{}:
	default:
	}
	return nil
}
func (s *Server) publicMonitorState() map[string]any {
	s.monitorMu.Lock()
	cfg, persistent := s.monitorConfig, s.monitorKeyPersistent
	var next *time.Time
	if s.monitorDone != nil && s.monitorNextCheckAt != nil {
		value := *s.monitorNextCheckAt
		next = &value
	}
	s.monitorMu.Unlock()
	st, mc := s.monitor.PublicState(), s.monitor.Configuration()
	persistence := "memory_only"
	if persistent {
		persistence = "secure_or_environment"
	}
	if !st.Enabled {
		next = nil
	}
	return map[string]any{"enabled": st.Enabled, "running": s.monitor.Running(), "configured": cfg.BaseURL != "" && cfg.APIKey != "", "baseUrl": st.BaseURL, "groupIds": st.GroupIDs, "excludedPlanTypes": st.ExcludedPlanTypes, "openaiRequestTimezone": st.OpenAIRequestTimezone, "intervalMinutes": monitorPublicMinutes(mc.Interval), "cooldownMinutes": monitorPublicMinutes(mc.Cooldown), "lastResult": st.LastResult, "lastError": st.LastError, "lastCheckAt": st.LastCheckAt, "nextCheckAt": next, "credentialPersistence": persistence}
}
func (s *Server) StartMonitor(parent context.Context) {
	s.stopMonitor()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.monitorMu.Lock()
	s.monitorCancel, s.monitorDone = cancel, done
	s.monitorMu.Unlock()
	go func() {
		defer close(done)
		defer s.clearMonitorDeadline(done)
		s.monitorOnce(ctx)
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		defer timer.Stop()
		s.resetMonitorTimer(ctx, done, timer)
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.monitorWake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				s.clearMonitorDeadline(done)
				s.monitorOnce(ctx)
				s.resetMonitorTimer(ctx, done, timer)
			case <-timer.C:
				s.clearMonitorDeadline(done)
				s.monitorOnce(ctx)
				s.resetMonitorTimer(ctx, done, timer)
			}
		}
	}()
}
func (s *Server) stopMonitor() {
	s.monitorMu.Lock()
	cancel, done := s.monitorCancel, s.monitorDone
	s.monitorCancel, s.monitorDone = nil, nil
	s.monitorNextCheckAt = nil
	s.monitorMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *Server) clearMonitorDeadline(done chan struct{}) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	if s.monitorDone == done {
		s.monitorNextCheckAt = nil
	}
}

func (s *Server) resetMonitorTimer(ctx context.Context, done chan struct{}, timer *time.Timer) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	cfg := s.monitor.Configuration()
	deadline := time.Now().UTC().Add(cfg.Interval)
	timer.Reset(cfg.Interval)
	if s.monitorDone != done {
		return
	}
	s.monitorNextCheckAt = nil
	if cfg.Enabled && ctx.Err() == nil {
		s.monitorNextCheckAt = &deadline
	}
}

func (s *Server) monitorOnce(ctx context.Context) {
	if s.monitor.Configuration().Enabled {
		_, _ = s.monitorCheck(ctx, "timer")
	}
}
func (s *Server) monitorCheck(ctx context.Context, trigger string) (sub2api.MonitorSummary, error) {
	// A pass may perform several sequential remote updates and account pages.
	// Each Sub2API HTTP request owns its timeout, as in the legacy console;
	// the caller still controls cancellation and an explicit overall deadline.
	s.syncOutputs(ctx, true)
	if err := ctx.Err(); err != nil {
		return sub2api.MonitorSummary{}, err
	}
	return s.monitor.Check(ctx, trigger)
}
func (s *Server) monitorHooks(cfg sub2api.Config) sub2api.MonitorHooks {
	backend := sub2api.BackendIdentity(cfg.BaseURL)
	return sub2api.MonitorHooks{
		LocalPlanType: func(_ context.Context, email string) string {
			if job, ok := s.jobs.FindByEmail(email); ok {
				return accountplan.Normalize(job.PlanType)
			}
			return "unknown"
		},
		Evaluate: func(ctx context.Context, email string, accounts []sub2api.RemoteAccount) (sub2api.RepairDecision, error) {
			job, ok := s.jobs.FindByEmail(email)
			if !ok {
				return sub2api.RepairDecision{Action: sub2api.ActionMissing}, nil
			}
			if autoRepairPlanExcluded(job, accounts, cfg.ExcludedPlanTypes) {
				return sub2api.RepairDecision{Action: sub2api.ActionExcluded}, nil
			}
			if err := autoRepairAuthorizationCompatible(job, accounts); err != nil {
				return sub2api.RepairDecision{Action: sub2api.ActionIneligible, Reason: err.Error()}, nil
			}
			decision := func(action sub2api.RepairAction) sub2api.RepairDecision {
				return sub2api.RepairDecision{Action: action, ClearPermanent: !job.AutoRepairBlocked && job.Status == jobs.StatusSucceeded && job.ResultSaved}
			}
			if job.AutoRepairBlocked {
				return decision(sub2api.ActionBlocked), nil
			}
			if job.Status == jobs.StatusQueued || isActiveStatus(job.Status) {
				return decision(sub2api.ActionBusy), nil
			}
			if len(job.AutoRepairPendingAccountIDs) > 0 && job.ResultSaved && job.Status == jobs.StatusSucceeded {
				return decision(sub2api.ActionBusy), nil
			}
			if cooldown := s.monitor.Configuration().Cooldown; cooldown > 0 && job.AutoRepairLastAttemptAt != nil && time.Since(*job.AutoRepairLastAttemptAt) < cooldown {
				return decision(sub2api.ActionCooldown), nil
			}
			if !job.LastAuthAutomated {
				return decision(sub2api.ActionIneligible), nil
			}
			input, _ := s.jobs.InputCopy(job.ID)
			password, _ := input["password"].(string)
			mail, _ := input["mailApiUrl"].(string)
			totp, _ := input["totpSecret"].(string)
			if saved, err := s.jobs.LoadCredentialsContext(ctx, job.ID); err == nil {
				if password == "" {
					password = saved.Password
				}
				if mail == "" {
					mail = saved.MailAPIURL
				}
				if totp == "" {
					totp = saved.TOTPSecret
				}
			}
			if eligible, _ := repairEligibility(job, password != "", mail != "", totp != ""); !eligible {
				return decision(sub2api.ActionIneligible), nil
			}
			if (job.TOTPKnownEnabled && totp == "") || job.SecurityCheckRequired {
				return decision(sub2api.ActionIneligible), nil
			}
			return decision(sub2api.ActionStart), nil
		},
		StartRepair: func(ctx context.Context, email string, accounts []sub2api.RemoteAccount) error {
			job, ok := s.jobs.FindByEmail(email)
			if !ok {
				return errors.New("local task not found")
			}
			// Recheck immediately before queueing, including updated local plan
			// metadata since Evaluate. Manual relogin never calls this hook.
			if autoRepairPlanExcluded(job, accounts, cfg.ExcludedPlanTypes) {
				return sub2api.ErrPlanExcluded
			}
			if err := autoRepairAuthorizationCompatible(job, accounts); err != nil {
				return err
			}
			ids := []int{}
			seen := map[int]bool{}
			for _, account := range accounts {
				id := int(account.ID)
				if id > 0 && !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
			_, err := s.jobs.QueueAutoRepair(job.ID, ids, backend)
			return err
		},
		RetryPending: func(ctx context.Context) (int, error) {
			updated := 0
			var lastErr error
			for _, job := range consoleJobs(s.jobs.List()) {
				if ctx.Err() != nil {
					return updated, ctx.Err()
				}
				if job.AutoRepairBlocked {
					continue
				}
				if job.Status != jobs.StatusSucceeded || !job.ResultSaved || len(job.AutoRepairPendingAccountIDs) == 0 || (job.AutoRepairBackend != backend && job.AutoRepairBackend != cfg.BaseURL) {
					continue
				}
				// Failed transfers respect the cooldown; freshly completed authorization
				// is sent immediately, including after a process restart.
				if cooldown := s.monitor.Configuration().Cooldown; cooldown > 0 && job.AutoRepairLastError != "" && job.AutoRepairLastAttemptAt != nil && time.Since(*job.AutoRepairLastAttemptAt) < cooldown {
					continue
				}
				n, err := s.syncRepairOutput(ctx, cfg, job)
				updated += n
				if err != nil {
					lastErr = err
				}
			}
			return updated, lastErr
		},
	}
}

func autoRepairAuthorizationCompatible(job *jobs.Job, accounts []sub2api.RemoteAccount) error {
	localMethod, err := jobs.NormalizeAuthorizationMethod(job.AuthorizationMethod)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		remoteMethod, err := jobs.AuthorizationMethodForClientIdentity(account.Credentials, account.Extra)
		if err != nil {
			return err
		}
		if remoteMethod != "" && remoteMethod != localMethod {
			return errors.New("本地与 Sub2API 账号的授权方式不一致，请手动选择授权方式后同步")
		}
	}
	return nil
}

func autoRepairPlanExcluded(job *jobs.Job, accounts []sub2api.RemoteAccount, excluded []string) bool {
	if len(accounts) == 0 {
		return sub2api.PlanExcluded(job.PlanType, excluded)
	}
	for _, account := range accounts {
		plan := accountplan.Normalize(account.PlanType)
		if plan == "unknown" {
			plan = accountplan.Normalize(job.PlanType)
		}
		if sub2api.PlanExcluded(plan, excluded) {
			return true
		}
	}
	return false
}
func (s *Server) syncRepairOutput(ctx context.Context, cfg sub2api.Config, job *jobs.Job) (int, error) {
	if err := s.jobs.CheckAutoRepair(job); err != nil {
		return 0, err
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, job.ID, "sub2api-import-oauth.json"))
	if err != nil || json.Unmarshal(data, &doc) != nil || len(doc.Accounts) == 0 {
		return 0, errors.New("auto-repair output is unavailable")
	}
	var selected map[string]any
	for _, account := range doc.Accounts {
		if strings.EqualFold(sub2api.AccountEmail(account), strings.TrimSpace(job.Email)) {
			selected = account
			break
		}
	}
	// Legacy single-account exports may omit identity fields. Do not apply that
	// fallback to a mixed-account file or to an explicitly different account.
	if selected == nil && len(doc.Accounts) == 1 && sub2api.AccountEmail(doc.Accounts[0]) == "" {
		selected = doc.Accounts[0]
	}
	local, _ := selected["credentials"].(map[string]any)
	if len(local) == 0 {
		return 0, errors.New("auto-repair output has no credentials for the task account")
	}
	// Carry the validated task identity into the remote identity check even
	// when the legacy export stores its email only in name or extra.
	local["email"] = strings.ToLower(strings.TrimSpace(job.Email))
	cfg.OnRetry = s.sub2APIRetryLogger(job.ID)
	client := sub2api.New(cfg)
	pending := append([]int(nil), job.AutoRepairPendingAccountIDs...)
	updated := 0
	var lastErr error
	current := job
	guard := func() error { return s.jobs.CheckAutoRepair(current) }
	for _, id := range job.AutoRepairPendingAccountIDs {
		err = client.SyncAccountCredentialsGuarded(ctx, id, local, cfg.CodexFingerprintMode, guard)
		if errors.Is(err, jobs.ErrAutoRepairSuperseded) {
			return updated, err
		}
		if err != nil && !errors.Is(err, sub2api.ErrAccountMissing) {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if err == nil {
			updated++
		}
		left := []int{}
		for _, n := range pending {
			if n != id {
				left = append(left, n)
			}
		}
		pending = left
		current, err = s.jobs.CommitAutoRepair(current, pending, sub2api.SafeError(lastErr), len(pending) == 0)
		if err != nil {
			return updated, err
		}
	}
	if len(pending) > 0 {
		if _, err := s.jobs.CommitAutoRepair(current, pending, sub2api.SafeError(lastErr), false); err != nil {
			return updated, err
		}
	}
	return updated, lastErr
}
