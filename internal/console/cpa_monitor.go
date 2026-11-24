package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/cpa"
	"tosub2-go-port/internal/jobs"
)

type cpaMonitorOptions struct {
	MonitorEnabled    bool     `json:"monitorEnabled"`
	ExcludedPlanTypes []string `json:"excludedPlanTypes"`
	IntervalMinutes   int      `json:"intervalMinutes"`
	CooldownMinutes   int      `json:"cooldownMinutes"`
}

func defaultCPAMonitorOptions() cpaMonitorOptions {
	return cpaMonitorOptions{ExcludedPlanTypes: []string{}, IntervalMinutes: 5, CooldownMinutes: 5}
}

func (o cpaMonitorOptions) clone() cpaMonitorOptions {
	o.ExcludedPlanTypes = append([]string{}, o.ExcludedPlanTypes...)
	return o
}

func (o *cpaMonitorOptions) normalize() error {
	if o.IntervalMinutes < 1 || o.IntervalMinutes > 1440 || o.CooldownMinutes < 0 || o.CooldownMinutes > 1440 {
		return errors.New("CPA 巡检间隔须为 1 到 1440 分钟，重登冷却须为 0 到 1440 分钟的整数，0 表示不限制冷却")
	}
	if len(o.ExcludedPlanTypes) > 32 {
		return errors.New("CPA 排除套餐选项过多")
	}
	values, seen := []string{}, map[string]bool{}
	for _, value := range o.ExcludedPlanTypes {
		plan := accountplan.Normalize(value)
		if plan == "unknown" && !strings.EqualFold(strings.TrimSpace(value), "unknown") {
			return errors.New("CPA 排除套餐包含无法识别的选项")
		}
		if !seen[plan] {
			values = append(values, plan)
			seen[plan] = true
		}
	}
	o.ExcludedPlanTypes = values
	return nil
}

type cpaMonitorSummary struct {
	Checked      int `json:"checked"`
	Started      int `json:"started"`
	ExcludedPlan int `json:"excludedPlan"`
	Busy         int `json:"busy"`
	Cooldown     int `json:"cooldown"`
	Ineligible   int `json:"ineligible"`
	Missing      int `json:"missing"`
	Failed       int `json:"failed"`
}

// All fields are protected by cpaMu, including scheduler and explicit checks.
type cpaMonitorRuntime struct {
	closed, running, uncertain bool
	cancel, checkCancel        context.CancelFunc
	done, checkDone            chan struct{}
	wake                       chan struct{}
	lastCheckAt, nextCheckAt   *time.Time
	lastError                  string
	lastResult                 cpaMonitorSummary
}

var (
	errCPAMonitorDisabled = errors.New("请先保存并启用 CPA 异常自动重登")
	errCPAMonitorBusy     = errors.New("CPA 巡检正在进行，请稍后查看结果")
	errCPAMonitorChanged  = errors.New("CPA 配置已变化，本次巡检已停止，请查看最新状态")
)

// A credential or scheduling change invalidates queued automatic work. Only
// the digest enters job metadata; the frozen management key remains private.
func cpaAutoRepairBackend(cfg cpa.Config) string {
	data, _ := json.Marshal([]any{cfg.BaseURL, cfg.APIKey, cfg.Priority, cfg.EffectiveWeight()})
	digest := sha256.Sum256(data)
	return "cpa:" + hex.EncodeToString(digest[:])
}

func (s *Server) cpaConfiguredLocked() bool {
	return s.cpa.BaseURL != "" && s.cpa.key != "" && s.cpa.loadError == "" && !s.cpaMonitor.uncertain
}

func (s *Server) cpaConfigLocked() cpa.Config {
	return cpa.Config{BaseURL: s.cpa.BaseURL, APIKey: s.cpa.key, UploadOptions: s.cpa.UploadOptions.Clone()}
}

func (s *Server) updateCPAAutoRepairPolicyLocked() {
	if s.jobs == nil {
		return
	}
	s.jobs.SetCPAAutoRepairPolicy(jobs.AutoRepairPolicy{
		Enabled: s.cpa.MonitorEnabled && s.cpaConfiguredLocked() && !s.cpaMonitor.closed && s.poolMonitorAllows("cpa"),
		Backend: cpaAutoRepairBackend(s.cpaConfigLocked()), ExcludedPlanTypes: s.cpa.ExcludedPlanTypes,
	})
}

func (s *Server) publicCPAMonitorState() map[string]any {
	s.cpaMu.Lock()
	defer s.cpaMu.Unlock()
	var next, last *time.Time
	if s.cpaMonitor.nextCheckAt != nil && s.cpa.MonitorEnabled && s.cpaConfiguredLocked() && !s.cpaMonitor.closed {
		value := *s.cpaMonitor.nextCheckAt
		next = &value
	}
	if s.cpaMonitor.lastCheckAt != nil {
		value := *s.cpaMonitor.lastCheckAt
		last = &value
	}
	persistence, configurationError := "memory_only", s.cpa.loadError
	if s.cpa.persistent {
		persistence = "secure"
	}
	if s.cpaMonitor.uncertain {
		configurationError = "CPA 配置持久化未确认，自动修复已暂停；请核对后重新保存"
	}
	return map[string]any{"revision": s.cpa.Revision, "enabled": s.cpa.MonitorEnabled, "configured": s.cpaConfiguredLocked(),
		"running": s.cpaMonitor.running, "intervalMinutes": s.cpa.IntervalMinutes, "cooldownMinutes": s.cpa.CooldownMinutes,
		"excludedPlanTypes": append([]string{}, s.cpa.ExcludedPlanTypes...), "nextCheckAt": next, "lastCheckAt": last,
		"lastError": s.cpaMonitor.lastError, "lastResult": s.cpaMonitor.lastResult,
		"credentialPersistence": persistence, "configurationError": configurationError}
}

func (s *Server) cpaMonitorAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/cpa/monitor" && r.Method == http.MethodPost {
		s.saveCPAMonitorSettings(w, r)
		return
	}
	method := http.MethodGet
	if strings.HasSuffix(r.URL.Path, "/check") {
		method = http.MethodPost
	}
	if r.Method != method {
		allow := method
		if r.URL.Path == "/api/cpa/monitor" {
			allow = "GET, POST"
		}
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if method == http.MethodPost {
		_, err := s.cpaMonitorCheck(r.Context())
		if err != nil {
			status, message := http.StatusBadGateway, cpa.SafeListError(err)
			outcome, uncertain := "unknown", true
			for _, known := range []error{errCPAMonitorDisabled, errCPAMonitorBusy, errCPAMonitorChanged} {
				if errors.Is(err, known) {
					status, message = http.StatusConflict, known.Error()
					if known != errCPAMonitorChanged {
						outcome, uncertain = "failed", false
					}
					break
				}
			}
			state := s.publicCPAMonitorState()
			state["error"] = message
			// Queue admission may have completed before a later read/cancel fails.
			// The UI must refresh state instead of automatically repeating this POST.
			state["outcome"], state["uncertain"] = outcome, uncertain
			writeJSON(w, status, state)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.publicCPAMonitorState())
}

func (s *Server) cpaMonitorCheck(parent context.Context) (summary cpaMonitorSummary, err error) {
	release, err := s.maintenance.Enter()
	if err != nil {
		return summary, err
	}
	defer release()
	if parent == nil {
		parent = context.Background()
	}
	s.cpaMu.Lock()
	if s.cpaMonitor.closed || !s.cpa.MonitorEnabled || !s.cpaConfiguredLocked() || !s.poolMonitorAllows("cpa") {
		s.cpaMu.Unlock()
		return summary, errCPAMonitorDisabled
	}
	if s.cpaMonitor.running {
		s.cpaMu.Unlock()
		return summary, errCPAMonitorBusy
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.cpaMonitor.running, s.cpaMonitor.checkCancel, s.cpaMonitor.checkDone = true, cancel, done
	cfg, options, revision := s.cpaConfigLocked(), s.cpa.cpaMonitorOptions.clone(), s.cpa.Revision
	s.cpaMu.Unlock()
	defer func() {
		cancel()
		s.cpaMu.Lock()
		s.cpaMonitor.running, s.cpaMonitor.checkCancel = false, nil
		now := time.Now().UTC()
		s.cpaMonitor.lastCheckAt, s.cpaMonitor.lastResult = &now, summary
		s.cpaMonitor.lastError = cpa.SafeListError(err)
		if errors.Is(err, errCPAMonitorChanged) {
			s.cpaMonitor.lastError = errCPAMonitorChanged.Error()
		}
		if err == nil && summary.Failed > 0 {
			s.cpaMonitor.lastError = "部分异常账号未能加入重登队列，请查看账号状态"
		}
		close(done)
		s.cpaMu.Unlock()
	}()
	files, err := cpa.New(cfg).ListAuthFiles(ctx)
	if err != nil {
		return summary, err
	}
	seen := map[string]bool{}
	for _, remote := range files {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		summary.Checked++
		if !remote.NeedsRelogin {
			continue
		}
		job, ok := s.findCPAMonitorJob(ctx, remote)
		if !ok {
			summary.Missing++
			continue
		}
		if seen[job.ID] {
			summary.Busy++
			continue
		}
		if accountplan.AutomaticRepairExcluded(job.PlanType, []string{remote.PlanType}, options.ExcludedPlanTypes) {
			summary.ExcludedPlan++
			continue
		}
		if job.Status == jobs.StatusQueued || isActiveStatus(job.Status) || len(job.AutoRepairPendingAccountIDs) > 0 {
			summary.Busy++
			continue
		}
		if job.AutoRepairSyncPaused || job.RecoveryPaused || job.SecurityCheckRequired {
			summary.Ineligible++
			continue
		}
		if options.CooldownMinutes > 0 && job.AutoRepairLastAttemptAt != nil && time.Since(*job.AutoRepairLastAttemptAt) < time.Duration(options.CooldownMinutes)*time.Minute {
			summary.Cooldown++
			continue
		}
		if !s.cpaRepairEligible(ctx, job) {
			summary.Ineligible++
			continue
		}
		prepared, prepareErr := s.prepareCPAAccount(ctx, job)
		if prepareErr != nil {
			summary.Ineligible++
			continue
		}
		var identity struct {
			AccountID string `json:"account_id"`
		}
		if json.Unmarshal(prepared.file.Data, &identity) != nil || !remote.MatchesIdentity(job.Email, identity.AccountID) || remote.Filename != prepared.file.Filename {
			summary.Ineligible++
			continue
		}
		grantPlan := accountplan.FromAccount(prepared.account)
		if accountplan.AutomaticRepairExcluded(job.PlanType, []string{remote.PlanType, grantPlan}, options.ExcludedPlanTypes) {
			summary.ExcludedPlan++
			continue
		}
		seen[job.ID] = true
		values := map[string]any{"uploadTarget": jobs.UploadTargetCPA, "uploadAfterRelogin": true,
			"authorizationMethod": jobs.AuthorizationMethodChatGPT, "cpaRepairAccountId": identity.AccountID,
			"cpaConfig": map[string]any{"baseUrl": cfg.BaseURL, "adminApiKey": cfg.APIKey, "priority": cfg.Priority, "weight": cfg.EffectiveWeight()}}
		s.cpaMu.Lock()
		if s.cpaMonitor.closed || !s.cpa.MonitorEnabled || !s.cpaConfiguredLocked() || !s.poolMonitorAllows("cpa") || s.cpa.Revision != revision || ctx.Err() != nil {
			s.cpaMu.Unlock()
			return summary, errCPAMonitorChanged
		}
		_, queueErr := s.jobs.QueueCPAAutoRepair(job.ID, cpaAutoRepairBackend(cfg), values, remote.PlanType, grantPlan)
		s.cpaMu.Unlock()
		if queueErr != nil {
			summary.Failed++
		} else {
			summary.Started++
		}
	}
	return summary, nil
}

func (s *Server) findCPAMonitorJob(ctx context.Context, remote cpa.RemoteAuthFile) (*jobs.Job, bool) {
	if remote.Email != "" {
		return s.jobs.FindByEmail(remote.Email)
	}
	// Older CPA listings may omit email. Match the exact filename derived from
	// a committed local grant, never the display label or a partial substring.
	for _, job := range consoleJobs(s.jobs.List()) {
		if ctx.Err() != nil {
			break
		}
		prepared, err := s.prepareCPAAccount(ctx, job)
		if err == nil && prepared.file.Filename == remote.Filename {
			return job, true
		}
	}
	return nil, false
}

func (s *Server) cpaRepairEligible(ctx context.Context, job *jobs.Job) bool {
	method, err := jobs.NormalizeAuthorizationMethod(job.AuthorizationMethod)
	if err != nil || method != jobs.AuthorizationMethodChatGPT {
		return false
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
	eligible, _ := repairEligibility(job, password != "", mail != "", totp != "")
	return eligible && (!job.TOTPKnownEnabled || totp != "")
}

func (s *Server) startCPAMonitor(parent context.Context) {
	s.cpaMu.Lock()
	defer s.cpaMu.Unlock()
	if s.cpaMonitor.closed {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.cpaMonitor.cancel, s.cpaMonitor.done = cancel, done
	// The initial pass uses the newest saved settings; consume stale wakeups.
	select {
	case <-s.cpaMonitor.wake:
	default:
	}
	go func() {
		defer func() {
			s.cpaMu.Lock()
			if s.cpaMonitor.done == done {
				s.cpaMonitor.nextCheckAt = nil
			}
			close(done)
			s.cpaMu.Unlock()
		}()
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			_, _ = s.cpaMonitorCheck(ctx)
			s.cpaMu.Lock()
			interval := time.Duration(s.cpa.IntervalMinutes) * time.Minute
			if interval <= 0 {
				interval = 5 * time.Minute
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(interval)
			s.cpaMonitor.nextCheckAt = nil
			if s.cpa.MonitorEnabled && s.cpaConfiguredLocked() && !s.cpaMonitor.closed && s.poolMonitorAllows("cpa") && ctx.Err() == nil {
				next := time.Now().UTC().Add(interval)
				s.cpaMonitor.nextCheckAt = &next
			}
			s.cpaMu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-s.cpaMonitor.wake:
			}
			s.cpaMu.Lock()
			s.cpaMonitor.nextCheckAt = nil
			s.cpaMu.Unlock()
		}
	}()
}

func (s *Server) stopCPAMonitor() {
	s.cpaMu.Lock()
	cancel, done := s.cpaMonitor.cancel, s.cpaMonitor.done
	s.cpaMonitor.cancel, s.cpaMonitor.done, s.cpaMonitor.nextCheckAt = nil, nil, nil
	s.cpaMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *Server) closeCPAMonitorContext(ctx context.Context) error {
	s.cpaMu.Lock()
	s.cpaMonitor.closed, s.cpaMonitor.nextCheckAt = true, nil
	cancel, done, checkCancel, checkDone := s.cpaMonitor.cancel, s.cpaMonitor.done, s.cpaMonitor.checkCancel, s.cpaMonitor.checkDone
	s.updateCPAAutoRepairPolicyLocked()
	s.cpaMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if checkCancel != nil {
		checkCancel()
	}
	for _, wait := range []chan struct{}{done, checkDone} {
		if wait != nil {
			select {
			case <-wait:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}
