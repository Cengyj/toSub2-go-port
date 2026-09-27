package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
)

type smsLease struct {
	provider        providers.SmsProvider
	lease           providers.NumberLease
	cancel          context.CancelFunc
	pollCancel      context.CancelFunc
	submitted       bool
	submittedAt     time.Time
	seen            map[string]bool
	pendingCode     string
	deadline        time.Time
	action          string
	restoredPending bool
	pollStopped     bool
	ready           bool
	retryPaused     bool
	generation      uint64
}

func smsProviderName(id string) string {
	switch id {
	case "luban":
		return "LubanSMS"
	case "smsbower":
		return "SMSBower"
	case "custom":
		return "自定义接码"
	default:
		return id
	}
}

func (s *Server) smsPollTimeout() time.Duration {
	if s.cfg.SMSPollTimeoutMS > 0 || s.cfg.SMSPollTimeoutSet {
		return time.Duration(s.cfg.SMSPollTimeoutMS) * time.Millisecond
	}
	return 10 * time.Minute
}

func (s *Server) smsPollInterval() time.Duration {
	if s.cfg.SMSPollIntervalMS > 0 {
		return time.Duration(s.cfg.SMSPollIntervalMS) * time.Millisecond
	}
	if s.cfg.SMSPollIntervalSet {
		// Node setTimeout(0), like negative delays, yields for a minimum of 1 ms.
		return time.Millisecond
	}
	return 3 * time.Second
}

func (s *Server) initializeSMS() {
	s.smsCtx, s.smsStop = context.WithCancel(context.Background())
	s.smsLeases = map[string]*smsLease{}
	s.smsAcquiring = map[string]bool{}
	s.smsPools = map[string]*providers.CustomPool{}
	s.restoreSMSLeases()
}
func (s *Server) customSMSPool(value any) (*providers.CustomPool, error) {
	var entries []string
	switch v := value.(type) {
	case string:
		entries = strings.Split(strings.ReplaceAll(v, "\r\n", "\n"), "\n")
	case []string:
		entries = v
	case []any:
		for _, x := range v {
			if line, ok := x.(string); ok {
				entries = append(entries, line)
			}
		}
	}
	clean := []string{}
	for _, line := range entries {
		if strings.TrimSpace(line) != "" {
			clean = append(clean, strings.TrimSpace(line))
		}
	}
	hash := sha256.Sum256([]byte(strings.Join(clean, "\n")))
	key := hex.EncodeToString(hash[:])
	s.smsMu.Lock()
	if pool := s.smsPools[key]; pool != nil {
		s.smsMu.Unlock()
		return pool, nil
	}
	s.smsMu.Unlock()

	pool := providers.NewCustomPool(clean)
	if err := pool.Validate(); err != nil {
		return nil, err
	}
	if c, err := s.smsVaultLoad("sms-pool-" + key); err == nil {
		var state providers.CustomPoolState
		if json.Unmarshal([]byte(c.Password), &state) == nil {
			if err := pool.RestorePrivateState(state); err != nil {
				return nil, err
			}
		}
	}
	s.smsMu.Lock()
	defer s.smsMu.Unlock()
	if existing := s.smsPools[key]; existing != nil {
		return existing, nil
	}
	if len(s.smsPools) >= 64 {
		return nil, errors.New("too many custom SMS configurations")
	}
	s.smsPools[key] = pool
	return pool, nil
}
func (s *Server) acquireSMSNumber(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		ProviderID string `json:"providerId"`
		Config     struct {
			APIKey       string `json:"apiKey"`
			ServiceID    string `json:"serviceId"`
			Service      string `json:"service"`
			Country      string `json:"country"`
			CountryLabel string `json:"countryLabel"`
			BaseURL      string `json:"baseUrl"`
			MaxPrice     string `json:"maxPrice"`
			Entries      any    `json:"entries"`
		} `json:"config"`
		APIKey    string `json:"apiKey"`
		ServiceID string `json:"serviceId"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.HasSuffix(r.URL.Path, "/luban-number") {
		// The old alias always selects Luban and reads only the top-level
		// credentials. Conflicting modern fields cannot change its provider.
		req.ProviderID = "luban"
		req.Config.APIKey, req.Config.ServiceID = req.APIKey, req.ServiceID
		req.Config.BaseURL = ""
	}
	job, ok := s.jobs.Get(id)
	if !ok {
		writeError(w, 404, "job not found")
		return
	}
	if job.Status != "phone" {
		writeError(w, 409, "只能在输入手机号阶段取号")
		return
	}
	if req.ProviderID == "" {
		req.ProviderID = "luban"
	}
	req.ProviderID = strings.ToLower(req.ProviderID)
	if req.Config.APIKey == "" {
		req.Config.APIKey = req.APIKey
	}
	if req.Config.ServiceID == "" {
		req.Config.ServiceID = req.ServiceID
	}
	if req.ProviderID != "custom" && !validProviderAPIKey(req.Config.APIKey) {
		writeError(w, 400, "请输入有效的接码平台 API Key")
		return
	}
	var p providers.SmsProvider
	serviceLabel := ""
	switch req.ProviderID {
	case "luban":
		serviceLabel = strings.TrimSpace(req.Config.ServiceID)
		p = &providers.HTTPProvider{ID: "luban", APIKey: req.Config.APIKey, Base: envOr(req.Config.BaseURL, os.Getenv("LUBAN_SMS_API_BASE")), Service: req.Config.ServiceID}
	case "smsbower":
		provider := providers.NewSMSBowerProvider(req.Config.APIKey, envOr(req.Config.BaseURL, os.Getenv("SMSBOWER_API_BASE")), req.Config.Service, req.Config.Country)
		provider.MaxPrice = req.Config.MaxPrice
		serviceLabel = req.Config.CountryLabel
		if serviceLabel == "" {
			serviceLabel = provider.Service + " / " + provider.Country
		}
		p = provider
	case "custom":
		pool, err := s.customSMSPool(req.Config.Entries)
		if err != nil {
			writeError(w, 400, safeProviderError(err))
			return
		}
		p = pool
		serviceLabel = fmt.Sprintf("%d 个号码", len(pool.PrivateState().Entries))
	default:
		writeError(w, 400, "unknown SMS provider")
		return
	}
	s.smsMu.Lock()
	if s.smsClosed || s.smsAcquiring[id] {
		s.smsMu.Unlock()
		writeError(w, 409, "取号操作正在进行或服务已关闭")
		return
	}
	s.smsAcquiring[id] = true
	s.smsWG.Add(1)
	s.smsMu.Unlock()
	defer func() { s.smsMu.Lock(); delete(s.smsAcquiring, id); s.smsMu.Unlock(); s.smsWG.Done() }()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.smsCtx, cancel)
	defer stop()
	s.releaseSMS(id)
	s.jobs.UpdateSMS(id, req.ProviderID, smsProviderName(req.ProviderID), "", "requesting", "", jobs.SanitizeSMSServiceLabel(serviceLabel, req.Config.APIKey))
	lease, err := p.GetNumber(ctx)
	if pool, ok := p.(*providers.CustomPool); ok {
		_ = s.saveCustomSMSPool(pool)
	}
	if err != nil {
		s.jobs.UpdateSMS(id, req.ProviderID, smsProviderName(req.ProviderID), "", "error", safeProviderError(err))
		writeError(w, 502, safeProviderError(err))
		return
	}
	release := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.Release(cleanup, lease)
	}
	job, ok = s.jobs.Get(id)
	if !ok || job.Status != "phone" || ctx.Err() != nil {
		release()
		writeError(w, 409, "任务已离开手机号输入阶段")
		return
	}
	if !s.installSMSLease(id, p, lease) {
		release()
		writeError(w, 503, "服务正在关闭")
		return
	}
	// Subscribe to state events before handing the number to the protocol.
	s.smsMu.Lock()
	installed := s.smsLeases[id]
	accepted := installed != nil && installed.lease.ID == lease.ID && s.jobs.InputForStage(id, "phone", map[string]any{"phone": lease.Number})
	if accepted {
		if current, exists := s.jobs.Get(id); exists {
			s.jobs.UpdateSMS(id, lease.Provider, smsProviderName(lease.Provider), current.CurrentPhone, "number_acquired", current.SMSError)
		}
	}
	s.smsMu.Unlock()
	if !accepted {
		s.releaseSMS(id)
		writeError(w, 409, "任务已离开手机号输入阶段")
		return
	}
	job, _ = s.jobs.Get(id)
	writeJSON(w, 200, map[string]any{"job": s.publicJob(job), "number": lease.Number})
}
func (s *Server) installSMSLease(id string, p providers.SmsProvider, lease providers.NumberLease) bool {
	return s.installSMSLeaseState(id, p, lease, nil)
}
func (s *Server) installSMSLeaseState(id string, p providers.SmsProvider, lease providers.NumberLease, saved *privateSMSState) bool {
	var deadline time.Time
	if saved != nil && !saved.Deadline.IsZero() {
		deadline = saved.Deadline
	}
	// A polling timeout pauses the attempt, not ownership of the number.
	// Manual input, resend, replacement and completion remain available.
	ctx, cancel := context.WithCancel(s.smsCtx)
	events, unsubscribe := s.jobs.Subscribe(512)
	l := &smsLease{provider: p, lease: lease, cancel: cancel, seen: map[string]bool{}, deadline: deadline}
	if saved != nil {
		for k, v := range saved.Seen {
			l.seen[k] = v
		}
		l.pendingCode = saved.PendingCode
		l.restoredPending = saved.PendingCode != ""
		l.action = saved.Action
		l.pollStopped = saved.PollStopped
	}
	if job, exists := s.jobs.Get(id); exists && job.PhoneRetryRequired {
		// A persisted uncertain write never authorizes replay of a saved OTP,
		// even if the protocol resumes before the event subscriber catches up.
		l.retryPaused = true
		l.pendingCode = ""
		l.restoredPending = false
		s.jobs.UpdateSMS(id, lease.Provider, smsProviderName(lease.Provider), lease.Number, "error", "手机号请求已暂停，请手动重试、输入验证码或更换号码")
	}
	s.smsMu.Lock()
	if s.smsClosed {
		s.smsMu.Unlock()
		cancel()
		unsubscribe()
		return false
	}
	s.smsLeases[id] = l
	s.smsWG.Add(1)
	s.smsMu.Unlock()
	if err := s.saveSMSLease(id, l); err != nil && lease.Provider != "fake" {
		s.jobs.UpdateSMS(id, lease.Provider, smsProviderName(lease.Provider), lease.Number, "number_acquired", "接码订单仅在本次运行中可用，无法安全持久保存")
	}
	go func() { defer s.smsWG.Done(); defer unsubscribe(); defer cancel(); s.runSMSLease(ctx, id, l, events) }()
	return true
}
func smsInputEvent(event jobs.JobEvent) bool {
	switch event.Status {
	case jobs.StatusWaitingInput, "awaiting_input":
		return true
	case "phone":
		return event.Stage == "phone"
	case "phone_otp", "sms_otp":
		return event.Stage == "phone_otp" || event.Stage == "sms_otp"
	default:
		return false
	}
}

func (s *Server) runSMSLease(ctx context.Context, id string, l *smsLease, events <-chan jobs.JobEvent) {
	interval := s.smsPollInterval()
	timer := time.NewTicker(interval)
	defer timer.Stop()
	s.smsMu.Lock()
	action := l.action
	s.smsMu.Unlock()
	if action != "" {
		s.finishSMS(id, l, action == "complete", "")
		return
	}
	for {
		select {
		case <-ctx.Done():
			s.smsMu.Lock()
			closed := s.smsClosed
			s.smsMu.Unlock()
			if closed {
				s.suspendSMS(id, l)
			} else {
				s.finishSMS(id, l, false, "短信轮询已停止")
			}
			return
		case event, open := <-events:
			if !open {
				s.finishSMS(id, l, false, "")
				return
			}
			if event.JobID != id {
				continue
			}
			if event.Stage == "phone_verified" {
				s.finishSMS(id, l, true, "")
				return
			}
			if event.Stage == "phone" && smsInputEvent(event) {
				current, exists := s.jobs.Get(id)
				if !exists || current.CurrentPhone != l.lease.Number || (!current.PhoneRetryRequired && current.Status == "phone") {
					s.finishSMS(id, l, false, "手机号未被接受，请选择其他号码")
					return
				}
			}
			if (event.Stage == "sms_otp" || event.Stage == "phone_otp") && smsInputEvent(event) {
				s.smsMu.Lock()
				if s.smsLeases[id] == l {
					// A zero-delay timer can observe phone_otp and submit before its
					// queued prompt arrives. Only a later prompt can reject/reset it.
					if l.submitted && !event.At.After(l.submittedAt) {
						s.smsMu.Unlock()
						continue
					}
					l.submitted = false
					if l.restoredPending {
						l.restoredPending = false
					} else {
						l.pendingCode = ""
					}
				}
				s.smsMu.Unlock()
			}
		case <-timer.C:
		}
		job, ok := s.jobs.Get(id)
		if !ok {
			s.finishSMS(id, l, false, "")
			return
		}
		if job.SMSStatus == "verified" {
			s.finishSMS(id, l, true, "")
			return
		}
		switch job.Status {
		case jobs.StatusFailed, jobs.StatusCanceled, jobs.StatusSucceeded, "interrupted", "reauth_required":
			s.finishSMS(id, l, false, "")
			return
		}
		if s.syncSMSPhoneRetry(id, l, job) {
			continue
		}
		s.smsMu.Lock()
		owned := s.smsLeases[id] == l
		submitted := l.submitted
		s.smsMu.Unlock()
		if !owned {
			return
		}
		if job.Status != "phone_otp" || submitted {
			continue
		}
		s.smsMu.Lock()
		if s.smsLeases[id] != l {
			s.smsMu.Unlock()
			return
		}
		if l.pendingCode != "" && !l.submitted {
			code := l.pendingCode
			l.submitted = true
			l.submittedAt = time.Now().UTC()
			l.restoredPending = false
			accepted := s.jobs.InputAutomaticSMS(id, code)
			if !accepted {
				if current, exists := s.jobs.Get(id); !exists || !current.PhoneRetryRequired {
					delete(l.seen, code)
				}
				l.pendingCode = ""
				l.submitted = false
			}
			s.smsMu.Unlock()
			if !accepted {
				_ = s.saveSMSLease(id, l)
			}
			continue
		}
		if l.pollStopped {
			s.smsMu.Unlock()
			continue
		}
		if current, exists := s.jobs.Get(id); !exists || current.PhoneRetryRequired || current.Status != "phone_otp" {
			s.smsMu.Unlock()
			continue
		}
		pollTimeout := s.smsPollTimeout()
		if l.deadline.IsZero() || pollTimeout <= 0 {
			l.deadline = time.Now().Add(pollTimeout)
		}
		if !time.Now().Before(l.deadline) {
			l.pollStopped = true
			s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "error", "等待平台短信超时，可以手动输入验证码、重新发送或更换手机号")
			s.smsMu.Unlock()
			_ = s.saveSMSLease(id, l)
			continue
		}
		deadline := l.deadline
		if limit := time.Now().Add(12 * time.Second); limit.Before(deadline) {
			deadline = limit
		}
		pollCtx, pollCancel := context.WithDeadline(ctx, deadline)
		l.pollCancel = pollCancel
		generation := l.generation
		markReady := !l.ready
		l.ready = true
		if markReady {
			s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "waiting_sms", "")
		}
		s.smsMu.Unlock()
		if markReady {
			// Readiness is advisory and follows protocol acceptance of the number.
			if ready, ok := l.provider.(providers.ReadyProvider); ok {
				_ = ready.MarkReady(pollCtx, l.lease)
			}
		}
		code, err := l.provider.GetCode(pollCtx, l.lease)
		pollErr := pollCtx.Err()
		pollCancel()
		s.smsMu.Lock()
		l.pollCancel = nil
		owned = s.smsLeases[id] == l
		current, exists := s.jobs.Get(id)
		eligible := owned && l.generation == generation && !l.submitted && pollErr == nil && exists && !current.PhoneRetryRequired && current.Status == "phone_otp"
		duplicate := eligible && err == nil && code != "" && l.seen[code]
		if eligible && err == nil && code != "" && !duplicate {
			l.seen[code] = true
			l.submitted = true
			l.submittedAt = time.Now().UTC()
			l.pendingCode = code
		} else {
			code = ""
		}
		stateChanged := code != ""
		if eligible {
			current, exists := s.jobs.Get(id)
			if exists && current.Status == "phone_otp" && current.SMSStatus != "verified" {
				switch {
				case code != "":
					// Persist pending input before delivering it to the runner below.
				case duplicate:
					l.pollStopped = true
					stateChanged = true
					s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "error", "接码平台仍返回已提交过的验证码，已停止重复提交")
				case providers.IsTerminal(err):
					l.pollStopped = true
					stateChanged = true
					s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "error", "接码平台已停止轮询，请检查配置、手动输入验证码或更换号码")
				case err != nil && !errors.Is(err, providers.ErrWaiting) && !errors.Is(err, context.Canceled):
					s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "waiting_sms", "短信暂未可用，正在重试")
				case err == nil || errors.Is(err, providers.ErrWaiting):
					s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "waiting_sms", "")
				}
			}
		}
		s.smsMu.Unlock()
		if !owned {
			return
		}
		if stateChanged {
			_ = s.saveSMSLease(id, l)
			if pool, ok := l.provider.(*providers.CustomPool); ok {
				_ = s.saveCustomSMSPool(pool)
			}
		}
		if code != "" {
			rejected := false
			s.smsMu.Lock()
			if s.smsLeases[id] == l && l.generation == generation && l.pendingCode == code {
				if s.jobs.InputAutomaticSMS(id, code) {
					s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "submitted", "")
				} else {
					if current, exists := s.jobs.Get(id); !exists || !current.PhoneRetryRequired {
						delete(l.seen, code)
					}
					l.pendingCode = ""
					l.submitted = false
					rejected = true
				}
			}
			s.smsMu.Unlock()
			if rejected {
				_ = s.saveSMSLease(id, l)
			}
		}

	}
}

// syncSMSPhoneRetry uses canonical manager state, not queued event order. The
// paused lease remains owned, but no pending OTP or provider poll can resume
// until a successful protocol event explicitly clears the marker.
func (s *Server) syncSMSPhoneRetry(id string, l *smsLease, job *jobs.Job) bool {
	s.smsMu.Lock()
	if s.smsLeases[id] != l {
		s.smsMu.Unlock()
		return true
	}
	changed := false
	if job.PhoneRetryRequired {
		if !l.retryPaused || l.pendingCode != "" || l.restoredPending {
			l.retryPaused = true
			l.pendingCode = ""
			l.restoredPending = false
			l.submitted = false
			l.generation++
			if l.pollCancel != nil {
				l.pollCancel()
			}
			s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "error", "手机号请求已暂停，请手动重试、输入验证码或更换号码")
			changed = true
		}
	} else if l.retryPaused {
		l.retryPaused = false
		l.pendingCode = ""
		l.restoredPending = false
		l.submitted = false
		l.pollStopped = false
		l.deadline = time.Time{}
		l.ready = false
		s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "number_acquired", "")
		changed = true
	}
	s.smsMu.Unlock()
	if changed {
		_ = s.saveSMSLease(id, l)
	}
	return job.PhoneRetryRequired
}

func (s *Server) prepareSMSPhoneInput(id, number string) {
	s.smsMu.Lock()
	lease := s.smsLeases[id]
	job, exists := s.jobs.Get(id)
	retain := lease != nil && exists && job.PhoneRetryRequired && job.CurrentPhone == number && lease.lease.Number == number
	s.smsMu.Unlock()
	if retain {
		s.pauseSMSSubmission(id, "r")
		return
	}
	s.releaseSMS(id)
}

func (s *Server) pauseSMSSubmission(id, value string) {
	s.smsMu.Lock()
	l := s.smsLeases[id]
	if l != nil {
		l.submitted = true
		l.submittedAt = time.Now().UTC()
		l.pendingCode = ""
		l.generation++
		if value == "r" {
			l.pollStopped = false
			l.ready = false
			l.deadline = time.Time{}
			s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "number_acquired", "")
		} else if value != "p" {
			s.jobs.UpdateSMS(id, l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number, "manual_submitted", "")
		}
		if value != "r" && value != "p" {
			l.seen[value] = true
			l.pendingCode = value
		}
		if l.pollCancel != nil {
			l.pollCancel()
		}
	}
	s.smsMu.Unlock()
	if l != nil {
		_ = s.saveSMSLease(id, l)
	}
}
func (s *Server) finishSMS(id string, l *smsLease, complete bool, message string) {
	smsPersistenceMu.Lock()
	s.smsMu.Lock()
	if s.smsLeases[id] != l {
		s.smsMu.Unlock()
		smsPersistenceMu.Unlock()
		return
	}
	l.action = "release"
	if complete {
		l.action = "complete"
	}
	state, persistErr := s.snapshotSMSLocked(id, l)
	delete(s.smsLeases, id)
	l.cancel()
	s.smsMu.Unlock()
	if persistErr == nil {
		_ = s.writeSMSState(state)
	}
	smsPersistenceMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var err error
	status := "idle"
	if complete {
		err = l.provider.Complete(ctx, l.lease)
		status = "completed"
	} else {
		err = l.provider.Release(ctx, l.lease)
		if message != "" {
			status = "error"
		}
	}
	if pool, ok := l.provider.(*providers.CustomPool); ok {
		_ = s.saveCustomSMSPool(pool)
	}
	if err != nil {
		message = "接码订单状态更新失败，将在重启恢复后重试"
		status = "error"
	} else {
		s.deleteSMSLeaseState(id, l.lease.ID)
	}
	s.smsMu.Lock()
	if s.smsLeases[id] == nil {
		providerID, providerName, number := l.lease.Provider, smsProviderName(l.lease.Provider), l.lease.Number
		if !complete {
			if job, exists := s.jobs.Get(id); exists {
				number = job.CurrentPhone
			}
		}
		if status == "idle" {
			providerID, providerName = "", ""
		}
		s.jobs.UpdateSMS(id, providerID, providerName, number, status, message)
	}
	s.smsMu.Unlock()
}
func (s *Server) releaseSMS(id string) {
	s.smsMu.Lock()
	l := s.smsLeases[id]
	s.smsMu.Unlock()
	if l != nil {
		s.finishSMS(id, l, false, "")
	}
}
func (s *Server) closeSMS() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.closeSMSContext(ctx)
}
func (s *Server) closeSMSContext(ctx context.Context) error {
	s.smsMu.Lock()
	s.smsClosed = true
	s.smsShutdownContext = ctx
	s.smsStop()
	s.smsMu.Unlock()
	done := make(chan struct{})
	go func() { s.smsWG.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
