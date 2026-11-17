package accountquality

import (
	"context"
	"errors"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

func validResult(r modelprobe.Result, c Settings) bool {
	return r.Model == c.Model && r.RuleVersion == modelprobe.RuleVersion && r.Total == questionCount && r.Score >= 0 && r.Score <= r.Total && !r.CheckedAt.IsZero() && r.DurationMS >= 0 &&
		(r.Verdict == "normal" && r.Score >= c.MinimumScore || r.Verdict == "degraded" && r.Score < c.MinimumScore)
}

func safeFailure(err error) (string, string) {
	code, message, _ := failureDetails(err)
	return code, message
}

func probeMessage(code string) string {
	diagnostic := modelprobe.DiagnoseFailure(&modelprobe.Error{Code: code})
	if diagnostic.Code == "probe_failed" {
		return ""
	}
	return diagnostic.Message
}

func failureDetails(err error) (string, string, int) {
	// Cancellation and the overall deadline still determine the outcome, but
	// must not erase a response status already observed by the probe client.
	observedStatus := 0
	var responseFailure *modelprobe.Error
	if errors.As(err, &responseFailure) && responseFailure != nil && responseFailure.HTTPStatus >= 100 && responseFailure.HTTPStatus <= 599 {
		observedStatus = responseFailure.HTTPStatus
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled", "检测已取消，历史有效结果保留", observedStatus
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "检测超时，未取得有效结果", observedStatus
	case errors.Is(err, ErrSuperseded):
		return "superseded", "账号授权或检测设置已改变，本次结果未采用", 0
	case errors.Is(err, ErrCachePersistence):
		return "persistence_failed", ErrCachePersistence.Error(), 0
	case errors.Is(err, ErrSettingsUnavailable) || errors.Is(err, ErrPersistence) || errors.Is(err, ErrPersistenceUncertain):
		return "settings_unavailable", ErrSettingsUnavailable.Error(), 0
	case errors.Is(err, ErrClosed):
		return "closed", ErrClosed.Error(), 0
	case errors.Is(err, ErrBusy):
		return "account_busy", "账号正在执行其他操作，未发起检测；请等待结束后重试", 0
	}
	diagnostic := modelprobe.DiagnoseFailure(err)
	if diagnostic.Code == "canceled" {
		return "cancelled", "检测已取消，历史有效结果保留", diagnostic.HTTPStatus
	}
	return diagnostic.Code, diagnostic.Message, diagnostic.HTTPStatus
}

func (s *Service) execute(f *flight, upload *UploadProbe) {
	executor := "manual"
	if upload != nil {
		executor = "upload"
	}
	select {
	case s.runGate <- struct{}{}:
	case <-f.ctx.Done():
		s.mu.Lock()
		s.cancelFlightLocked(f, "检测已取消")
		s.mu.Unlock()
		return
	}
	defer func() { <-s.runGate }()
	s.mu.Lock()
	if s.closed || s.flights[f.key] != f || f.executor != executor || f.ctx.Err() != nil {
		if s.flights[f.key] == f && f.executor == executor {
			s.cancelFlightLocked(f, "检测已取消")
		}
		s.mu.Unlock()
		return
	}
	f.running = true
	s.active++
	now := s.now().UTC()
	row := s.rows[f.id]
	row.Snapshot.LastAttempt.Status = "running"
	row.Snapshot.LastAttempt.Source = f.source
	row.Snapshot.LastAttempt.StartedAt = &now
	row.Snapshot.Persisted = false
	s.rows[f.id] = row
	s.version++
	s.mu.Unlock()
	var result modelprobe.Result
	var runErr error
	defer func() {
		if recover() != nil {
			runErr = errors.New("probe panic")
			s.complete(f, modelprobe.Result{}, runErr, nil)
		}
		s.mu.Lock()
		if s.flights[f.key] == f {
			delete(s.flights, f.key)
		}
		s.active--
		s.version++
		f.doneOnce.Do(func() { close(f.done) })
		s.mu.Unlock()
		f.cancel()
	}()
	var identity string
	var validate func() error
	var probe func(context.Context, modelprobe.Options) (modelprobe.Result, error)
	runCtx, cancel := context.WithTimeout(f.ctx, time.Duration(f.settings.TimeoutSeconds)*time.Second)
	defer cancel()
	if upload != nil {
		identity, validate, probe = upload.Identity, upload.Validate, upload.Run
	} else {
		lease, err := s.provider.Acquire(runCtx, f.id)
		if err != nil {
			s.complete(f, result, err, nil)
			return
		}
		if lease == nil {
			s.complete(f, result, errors.New("invalid lease"), nil)
			return
		}
		if lease.Release != nil {
			defer lease.Release()
		}
		if lease.Context == nil || lease.Release == nil {
			s.complete(f, result, errors.New("invalid lease"), nil)
			return
		}
		stop := context.AfterFunc(lease.Context, cancel)
		defer stop()
		identity, validate, probe = lease.Identity, lease.Validate, lease.Probe
	}
	if identity == "" || validate == nil || probe == nil {
		s.complete(f, result, errors.New("invalid probe"), nil)
		return
	}
	if err := validate(); err != nil {
		s.complete(f, result, ErrSuperseded, nil)
		return
	}
	s.mu.Lock()
	f.identity = digest(identity)
	s.mu.Unlock()
	// The durable running marker prevents a previous normal result from being
	// reused after a crash or a failed final write of a newer attempt.
	if err := s.persistAttempt(runCtx, f); err != nil {
		s.complete(f, result, err, nil)
		return
	}
	f.probed = runCtx.Err() == nil
	result, runErr = probe(runCtx, options(f.settings))
	if runCtx.Err() != nil {
		runErr = errors.Join(runCtx.Err(), runErr)
	}
	if runErr == nil && (validate() != nil || f.ctx.Err() != nil) {
		runErr = ErrSuperseded
	}
	if runErr == nil && (!validResult(result, f.settings) || result.CheckedAt.After(s.now().Add(time.Minute))) {
		runErr = &modelprobe.Error{Code: "invalid_response"}
	}
	s.complete(f, result, runErr, validate)
}

func (s *Service) persistAttempt(ctx context.Context, f *flight) error {
	if err := s.lockWrite(ctx); err != nil {
		return err
	}
	defer s.unlockWrite()
	s.mu.Lock()
	row := s.rows[f.id]
	if s.flights[f.key] != f || row.Snapshot.LastAttempt.CheckID != f.checkID || s.closed || s.settingsErr != nil || s.settings.Revision != f.settings.Revision {
		s.mu.Unlock()
		return ErrSuperseded
	}
	row.Snapshot = cloneSnapshot(row.Snapshot)
	row.Snapshot.Persisted = true
	s.mu.Unlock()
	if err := s.persist(ctx, snapshotFile(f.id), row); err != nil {
		return ErrCachePersistence
	}
	s.mu.Lock()
	current := s.rows[f.id]
	if current.Snapshot.LastAttempt.CheckID == f.checkID {
		current.Snapshot.Persisted = true
		s.rows[f.id] = current
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) complete(f *flight, result modelprobe.Result, runErr error, validate func() error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.lockWrite(ctx); err != nil {
		s.mu.Lock()
		f.err = ErrCachePersistence
		s.lastError = ErrCachePersistence.Error()
		// The durable running marker must remain untouched, but once the
		// runner exits its public attempt cannot stay running or persisted.
		// Preserve confirmed results/history and the normal retry cooldown.
		row := s.rows[f.id]
		if row.Snapshot.LastAttempt.CheckID == f.checkID {
			now := s.now().UTC()
			row.Snapshot.LastAttempt.FinishedAt = &now
			row.Snapshot.LastAttempt.Status = "failed"
			row.Snapshot.LastAttempt.ErrorCode = "persistence_failed"
			row.Snapshot.LastAttempt.HTTPStatus = 0
			row.Snapshot.LastAttempt.Message = ErrCachePersistence.Error()
			if runErr != nil {
				code, message, status := failureDetails(runErr)
				row.Snapshot.LastAttempt.ErrorCode, row.Snapshot.LastAttempt.Message, row.Snapshot.LastAttempt.HTTPStatus = code, message, status
				if code == "cancelled" || code == "superseded" {
					row.Snapshot.LastAttempt.Status = "cancelled"
				}
				f.err = errors.Join(ErrCachePersistence, runErr)
			}
			row.Snapshot.Persisted = false
			next := now.Add(time.Duration(f.settings.CooldownSeconds) * time.Second)
			row.Snapshot.NextAllowedAt = &next
			s.rows[f.id] = row
			s.version++
		}
		s.mu.Unlock()
		return
	}
	defer s.unlockWrite()
	accounts := accountMap(s.provider.Accounts())
	a, exists := accounts[f.id]
	if runErr == nil && (!exists || a.Key != f.key || a.Revision != f.accountRevision || validate == nil || validate() != nil) {
		runErr = ErrSuperseded
	}
	s.mu.Lock()
	row := s.rows[f.id]
	if row.Snapshot.LastAttempt.CheckID != f.checkID {
		s.mu.Unlock()
		f.err = ErrSuperseded
		return
	}
	if runErr == nil && (s.closed || s.settingsErr != nil || s.settings.Revision != f.settings.Revision || f.ctx.Err() != nil) {
		runErr = ErrSuperseded
	}
	now := s.now().UTC()
	row.Snapshot.LastAttempt.FinishedAt = &now
	row.Snapshot.LastAttempt.ErrorCode = ""
	row.Snapshot.LastAttempt.HTTPStatus = 0
	row.Snapshot.LastAttempt.Message = ""
	row.Snapshot.LastAttempt.Status = "success"
	if runErr != nil {
		code, message, status := failureDetails(runErr)
		row.Snapshot.LastAttempt.ErrorCode = code
		row.Snapshot.LastAttempt.HTTPStatus = status
		row.Snapshot.LastAttempt.Message = message
		row.Snapshot.LastAttempt.Status = "failed"
		if code == "cancelled" || code == "superseded" {
			row.Snapshot.LastAttempt.Status = "cancelled"
		}
	} else {
		r := result
		row.Snapshot.LastResult = &r
		row.IdentityHash = f.identity
		row.OptionsHash = optionsHash(f.settings)
		row.AccountRevision = f.accountRevision
	}
	if observation := completedHTTPObservation(result, runErr, row.Snapshot.LastAttempt.Status, f.settings.Model, now); observation != nil {
		row.HTTPObservation, row.HTTPObservationRevision = observation, f.accountRevision
	}
	row.Snapshot.HTTPObservation = nil
	previousHistory := normalizeProbeHistory(row.Snapshot.ProbeHistory)
	if f.probed && exists && a.Key == f.key && a.Revision == f.accountRevision {
		if item := candyHistoryEntry(row.Snapshot.LastAttempt, row.Snapshot.LastResult, f.settings.Model); item != nil {
			row.Snapshot.ProbeHistory = normalizeProbeHistory(append(row.Snapshot.ProbeHistory, *item))
		}
	}
	next := now.Add(time.Duration(f.settings.CooldownSeconds) * time.Second)
	row.Snapshot.NextAllowedAt = &next
	row.Snapshot.Persisted = true
	row.Snapshot = cloneSnapshot(row.Snapshot)
	s.mu.Unlock()
	err := s.persist(ctx, snapshotFile(f.id), row)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		row.Snapshot.ProbeHistory = previousHistory
		row.Snapshot.Persisted = false
		s.lastError = ErrCachePersistence.Error()
		if runErr == nil {
			row.Snapshot.LastAttempt.Status = "failed"
			row.Snapshot.LastAttempt.ErrorCode = "persistence_failed"
			row.Snapshot.LastAttempt.HTTPStatus = 0
			row.Snapshot.LastAttempt.Message = ErrCachePersistence.Error()
		}
		runErr = errors.Join(ErrCachePersistence, runErr)
	}
	s.rows[f.id] = row
	s.version++
	f.err = runErr
	if runErr == nil {
		f.result = cloneResult(&result)
	}
}

func (s *Service) freshLocked(id, identity string, c Settings) *modelprobe.Result {
	row := s.rows[id]
	r := row.Snapshot.LastResult
	now := s.now().UTC()
	if row.Snapshot.LastAttempt.Status != "success" || !row.Snapshot.Persisted || row.IdentityHash != digest(identity) || row.OptionsHash != optionsHash(c) || r == nil || !validResult(*r, c) || r.CheckedAt.After(now) || now.Sub(r.CheckedAt) > time.Duration(c.ResultTTLSeconds)*time.Second {
		return nil
	}
	return cloneResult(r)
}

func blocked(code string, cause error) (UploadDecision, error) {
	d := UploadDecision{ErrorCode: code, Message: gateMessage(code)}
	return d, &GateError{Code: code, Cause: cause}
}
func uploadOutcome(c Settings, result *modelprobe.Result, err error, cached bool) (UploadDecision, error) {
	if err != nil {
		code, msg := safeFailure(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrSuperseded) || code == "cancelled" {
			return blocked(code, err)
		}
		if errors.Is(err, ErrCachePersistence) {
			return blocked("persistence_failed", err)
		}
		// Failure policy applies to model observations, never missing durable
		// evidence, an unavailable policy, a closed owner or unsafe admission.
		var probe *modelprobe.Error
		probeFailure := errors.As(err, &probe) || errors.Is(err, context.DeadlineExceeded)
		if !probeFailure || code == "authorization_unavailable" || code == "authorization_expired" || code == "proxy_selection_required" || code == "invalid_credentials" || code == "invalid_options" || code == "unsafe_transport" || code == "transport_unavailable" || code == "unauthorized" || code == "redirect_blocked" {
			return blocked(code, err)
		}
		d := UploadDecision{Allowed: !c.BlockFailed, Checked: true, ErrorCode: code, Message: msg}
		if c.BlockFailed {
			return d, &GateError{Code: code, Cause: ErrUploadBlocked}
		}
		return d, nil
	}
	if result == nil {
		return blocked("probe_failed", ErrUploadBlocked)
	}
	d := UploadDecision{Allowed: result.Verdict != "degraded" || !c.BlockDegraded, Checked: true, Cached: cached, Result: cloneResult(result)}
	if !d.Allowed {
		d.ErrorCode = "degraded"
		d.Message = gateMessage("degraded")
		return d, &GateError{Code: "degraded", Cause: ErrUploadBlocked}
	}
	return d, nil
}

func (s *Service) CheckForUpload(ctx context.Context, id string, probe UploadProbe) (UploadDecision, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return blocked("cancelled", err)
	}
	c, err := s.settingsContext(ctx)
	if err != nil {
		return blocked("settings_unavailable", err)
	}
	if !c.Enabled || !c.BeforeUpload {
		return UploadDecision{Allowed: true}, nil
	}
	if probe.Identity == "" || probe.Validate == nil || probe.Run == nil {
		return blocked("superseded", ErrSuperseded)
	}
	if probe.Validate() != nil {
		return blocked("superseded", ErrSuperseded)
	}
	accounts := accountMap(s.provider.Accounts())
	a, ok := accounts[id]
	if !ok {
		return blocked("superseded", ErrSuperseded)
	}
	s.mu.Lock()
	if s.closed || s.ctx == nil || s.ctx.Err() != nil {
		s.mu.Unlock()
		return blocked("cancelled", ErrClosed)
	}
	if s.settingsErr != nil || s.settings.Revision != c.Revision {
		s.mu.Unlock()
		return blocked("settings_unavailable", ErrSettingsUnavailable)
	}
	if r := s.freshLocked(id, probe.Identity, c); r != nil {
		s.mu.Unlock()
		if probe.Validate() != nil {
			return blocked("superseded", ErrSuperseded)
		}
		latest, policyErr := s.settingsContext(ctx)
		if policyErr != nil {
			return blocked("settings_unavailable", policyErr)
		}
		if latest.Revision != c.Revision {
			return blocked("superseded", ErrSuperseded)
		}
		return uploadOutcome(c, r, nil, true)
	}
	f := s.flights[a.Key]
	execute := false
	if f != nil {
		if !f.running && f.executor == "manual" && f.id == id && optionsHash(f.settings) == optionsHash(c) {
			f.executor = "upload"
			f.source = "upload"
			f.settings = c
			f.accountRevision = a.Revision
			f.identity = digest(probe.Identity)
			execute = true
		} else if f.id != id || f.identity != digest(probe.Identity) || optionsHash(f.settings) != optionsHash(c) {
			s.mu.Unlock()
			return blocked("busy", ErrBusy)
		}
	} else {
		row := s.rows[id]
		if row.AccountRevision == a.Revision && row.Snapshot.NextAllowedAt != nil && s.now().Before(*row.Snapshot.NextAllowedAt) {
			s.mu.Unlock()
			return blocked("cooldown", ErrUploadBlocked)
		}
		if len(s.flights) >= 1000 {
			s.mu.Unlock()
			return blocked("busy", ErrBusy)
		}
		f = s.newFlightLocked(a, c, "upload")
		f.identity = digest(probe.Identity)
		execute = true
	}
	if execute {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if execute {
		func() {
			defer s.wg.Done()
			stop := context.AfterFunc(ctx, f.cancel)
			defer stop()
			s.execute(f, &probe)
		}()
	} else {
		select {
		case <-f.done:
		case <-ctx.Done():
			return blocked("cancelled", ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return blocked("cancelled", err)
	}
	if probe.Validate() != nil {
		return blocked("superseded", ErrSuperseded)
	}
	latest, settingsErr := s.settingsContext(ctx)
	if settingsErr != nil {
		return blocked("settings_unavailable", settingsErr)
	}
	if latest.Revision != c.Revision {
		return blocked("superseded", ErrSuperseded)
	}
	return uploadOutcome(c, f.result, f.err, false)
}
