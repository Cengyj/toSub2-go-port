package jobs

import (
	"context"
	"errors"
	"time"
	"tosub2-go-port/internal/resetlog"
	"tosub2-go-port/internal/store"
)

// executionAttempt owns a single runner context until the runner has returned.
// Cancellation invalidates input immediately but does not release the email
// lease; resource cleanup still belongs to that runner.
type executionAttempt struct {
	ctx             context.Context
	cancel          context.CancelFunc
	credentialEdits map[string]any
}

func (a *executionAttempt) canceled() bool {
	return a != nil && a.ctx != nil && a.ctx.Err() != nil
}

var ErrRunnerPanic = errors.New("TASK_RUNNER_PANIC: 操作异常中断；已保留授权和恢复证据，请核对本次结果后再操作")

// A panic is an interrupted attempt, never proof that a remote request was not
// sent. Preserve the normal result/recovery finalization and never log the
// panic value: it may contain a provider response or credentials.
func invokeRunner(runner Runner, ctx context.Context, job *Job, emit func(JobEvent)) (err error) {
	returned := false
	defer func() {
		if !returned {
			_ = recover()
			err = ErrRunnerPanic
		}
	}()
	err = runner(ctx, job, emit)
	returned = true
	return err
}

func (m *Manager) Start() {
	m.startOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closing || m.rootCtx.Err() != nil {
			return
		}
		// Shutdown takes the same lock before starting Wait: no Add can arrive
		// after the shutdown barrier has observed an empty scheduler.
		m.wg.Add(1)
		go m.schedule()
	})
}
func (m *Manager) schedule() {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		for !m.closing && m.rootCtx.Err() == nil && len(m.active) < m.max {
			index := m.nextRunnableLocked()
			if index < 0 {
				break
			}
			job := m.pending[index]
			m.pending = append(m.pending[:index], m.pending[index+1:]...)
			if reason := m.autoRepairSkipReasonLocked(job); reason != "" {
				m.skipAutoRepairLocked(job, reason)
				continue
			}
			before := cloneJob(job)
			now := time.Now().UTC()
			job.Status = StatusRunning
			job.StartedAt = &now
			job.UpdatedAt = now
			if job.SecurityActionReturnStatus != StatusImported || !isSuboperation(job.Operation) {
				job.Attempts++
			}
			event := m.applyEventLocked(JobEvent{JobID: job.ID, Status: job.Status, Stage: "start", At: now})
			m.persistLocked()
			if m.lastPersistError != nil {
				// This attempt never reached the runner. Keep its prior grant
				// and private recovery evidence and require explicit admission
				// after storage repair, even if the start record was replaced.
				*job = *before
				job.Status, job.OperationStatus, job.Stage = StatusFailed, "interrupted", "start_blocked"
				job.Error = "TASK_START_PERSISTENCE: 执行前状态保存失败，任务未开始；请检查存储后重试"
				if errors.Is(m.lastPersistError, store.ErrDurabilityUncertain) {
					job.Error = "TASK_START_UNCERTAIN: 执行前状态已替换但持久性未确认，任务未开始；请核对存储后重试"
				}
				job.OperationError, job.Prompt = job.Error, job.Error
				job.RecoveryPaused, job.UpdatedAt, job.FinishedAt = true, now, &now
				continue
			}
			ctx, cancel := context.WithCancel(m.rootCtx)
			m.active[job.ID] = &executionAttempt{ctx: ctx, cancel: cancel}
			if job.Email != "" {
				m.emailUse[emailKey(job.Email)] = job.ID
			}
			m.publishEventLocked(event)
			m.wg.Add(1)
			go m.run(ctx, cancel, cloneJob(job))
		}
		m.mu.Unlock()
		select {
		case <-m.rootCtx.Done():
			return
		case <-m.wake:
		}
	}
}
func (m *Manager) nextRunnableLocked() int {
	for i, job := range m.pending {
		if lease := m.accountQueries[emailKey(job.Email)]; lease != nil {
			lease.cancel()
			continue
		}
		if m.deleting[emailKey(job.Email)] || m.importing[emailKey(job.Email)] {
			continue
		}
		if job.Email == "" || m.emailUse[emailKey(job.Email)] == "" {
			return i
		}
	}
	return -1
}
func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, job *Job) {
	defer m.wg.Done()
	// Each attempt owns its context even when it finishes normally. Release
	// descendants and AfterFunc callbacks after final status classification,
	// so cleanup cannot turn a successful or failed action into a cancellation.
	defer cancel()
	m.mu.Lock()
	runner := m.runner
	if current := m.jobs[job.ID]; isAutomaticRepair(current) {
		if reason := m.autoRepairSkipReasonLocked(current); reason != "" && ctx.Err() == nil {
			delete(m.active, current.ID)
			delete(m.emailUse, emailKey(current.Email))
			if current.Attempts > 0 {
				current.Attempts--
			}
			m.skipAutoRepairLocked(current, reason)
			m.mu.Unlock()
			m.wakeScheduler()
			return
		}
	}
	m.mu.Unlock()
	if runner == nil {
		runner = func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
			emit(JobEvent{Stage: "ready", Status: StatusRunning})
			return nil
		}
	}
	// A runner may hand the callback to asynchronous work. Its authority ends
	// at cancellation or return, before a later operation can reuse this job ID.
	// Guard the lifetime with the same mutex as terminal state and admission.
	attemptDone := false
	attemptID := job.ID
	err := ctx.Err()
	if err == nil || !isAutomaticRepair(job) {
		err = invokeRunner(runner, ctx, job, func(e JobEvent) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if attemptDone {
				return
			}
			observationOnly := false
			if ctx.Err() != nil {
				// The runner still owns this attempt until it returns. Keep its
				// final *display-only* evidence, never a late input or state event.
				if observed, ok := finalProfileObservation(e, m.jobs[attemptID]); ok {
					e, observationOnly = observed, true
				} else {
					d, ok := e.Data[resetlog.DataKey].(*resetlog.Detail)
					if !job.ResetTOTP || !ok || d == nil || !d.Terminal || d.Safe() == nil {
						return
					}
				}
			}
			e.JobID = attemptID
			if e.At.IsZero() {
				e.At = time.Now().UTC()
			}
			if observationOnly {
				m.appendOperationLogLocked(m.jobs[attemptID], e)
			} else {
				e = m.applyEventLocked(e)
			}
			m.persistLocked()
			if m.lastPersistError == nil {
				m.publishEventLocked(e)
			}
		})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	attemptDone = true
	job = m.jobs[job.ID]
	credentialEditErr := m.applyDeferredCredentialEditsLocked(job, m.active[job.ID])
	delete(m.active, job.ID)
	if job.Email != "" {
		delete(m.emailUse, emailKey(job.Email))
	}
	now := time.Now().UTC()
	job.UpdatedAt = now
	job.FinishedAt = &now
	restart := m.restart[job.ID] && !errors.Is(err, ErrRunnerPanic) && credentialEditErr == nil
	delete(m.restart, job.ID)
	if job.TOTPRecoveryPending || job.TOTPCredentialInvalidated {
		job.Status, job.Error = StatusFailed, totpRecoveryDiagnostic(job, err)
		job.Prompt = "2FA 重置未完成，需要手动核实；不会自动重试移除或退出"
		if job.TOTPResetStatus == "disabling" || job.TOTPResetStatus == "disabled" {
			job.TOTPResetStatus = "unknown"
		}
	} else if (job.TOTPWorkflowStatus == "failed" || job.TOTPWorkflowStatus == "interrupted") && (job.ReloginAfterTOTP || job.UploadAfterTOTP) && totpOperation(job.Operation) && err != nil {
		job.Status, job.Error = StatusFailed, safeError(err)
	} else if job.ResetTOTP && err != nil {
		job.Status, job.Error = StatusFailed, safeError(err)
	} else if m.closing || m.rootCtx.Err() != nil {
		job.Status = "interrupted"
		job.Error = "服务关闭，任务已中断，请重新选择要执行的操作"
	} else if restart {
		job.Status = StatusQueued
		job.Operation = "full"
		initializeTOTPWorkflow(job, nil)
		job.LastOperationType, job.LastOperationAt = "reauthorize", now
		job.Stage = "queued"
		job.Prompt = "账号资料已更新，正在重新建立登录会话"
		job.QueuedAt = now
		job.FinishedAt = nil
		job.Error = ""
		m.pending = append(m.pending, job)
	} else if ctx.Err() != nil {
		// Only cancellation of this job's context is a task cancellation.
		// Browser/provider subcontexts have their own deadlines and cleanup;
		// preserve those operation failures instead of replacing them with
		// "canceled" and losing the actionable challenge/transport diagnostic.
		if !restoreCanceledSuboperation(job) {
			job.Status = StatusCanceled
			job.Error = "canceled"
			job.Prompt = "操作已取消"
		}
	} else if totpOperation(job.Operation) && err == nil && job.TOTPWorkflowStatus == "succeeded" && job.ReloginAfterTOTP {
		job.Status, job.Stage, job.Prompt = StatusSucceeded, "complete", "2FA 与重新授权流程已完成"
		if job.UploadAfterTOTP {
			job.Prompt = "2FA、重新授权和 " + uploadTargetLabel(job) + " 同步已完成"
		}
	} else if job.ResetTOTP && job.TOTPResetPhase == "credential_persisted" {
		completed := "2FA 已重置，新密钥已安全保存"
		if job.Operation == "complete-2fa" {
			completed = "2FA 本地保存已完成"
		}
		job.Status, job.Stage, job.Prompt = StatusSucceeded, "complete", completed
		if !job.ResultSaved {
			job.Status = "reauth_required"
			job.Prompt = completed + "；本地未保留可用授权文件，需要另行获取授权"
		}
		if job.SecurityActionReturnStatus == StatusImported {
			job.Status, job.Stage, job.QueuedAt = StatusImported, StatusImported, time.Time{}
			job.Prompt = completed + "，请选择要执行的功能"
		}
	} else if isSuboperation(job.Operation) {
		job.Status = StatusSucceeded
		if !job.ResultSaved {
			job.Status = "reauth_required"
		}
		if job.SecurityActionReturnStatus == StatusImported {
			job.Status, job.Stage, job.Prompt = StatusImported, StatusImported, "操作已结束，请选择要执行的功能"
			job.QueuedAt = time.Time{}
		}
		if err != nil {
			job.Error = safeError(err)
		}
	} else if err != nil {
		job.Status = StatusFailed
		job.Error = safeError(err)
	} else {
		job.Status = StatusSucceeded
		job.Stage = "complete"
	}
	// Keep the action outcome separate from retained authorization results.
	job.OperationStatus, job.OperationError = "succeeded", ""
	switch {
	case restart:
		beginOperationState(job)
		job.Prompt = "账号资料已更新，正在重新建立登录会话"
	case job.TOTPRecoveryPending || job.TOTPCredentialInvalidated:
		job.OperationStatus, job.OperationError = "failed", job.Error
	case m.closing || m.rootCtx.Err() != nil:
		job.OperationStatus, job.OperationError = "interrupted", job.Error
	case ctx.Err() != nil:
		job.OperationStatus = "canceled"
	case err != nil:
		job.OperationStatus, job.OperationError = "failed", safeError(err)
		if totpOperation(job.Operation) && job.TOTPWorkflowError != "" {
			job.OperationError = job.TOTPWorkflowError
			job.Error = job.TOTPWorkflowError
		}
	}
	if job.OperationStatus == "succeeded" {
		job.Error = ""
		job.Prompt = operationCompletionPrompt(job)
	} else if job.OperationStatus == "failed" || job.OperationStatus == "interrupted" {
		job.Prompt = job.OperationError
	}
	if isAutomaticRepair(job) && !m.deleting[emailKey(job.Email)] && (job.OperationStatus == "canceled" || job.OperationStatus == "interrupted") {
		clearAutoRepairTargets(job)
	}
	if pendingRefresh := m.pendingRefreshOperation(job); pendingRefresh != "" && err != nil {
		job.Operation, job.Stage = pendingRefresh, refreshRecoveryStage
		job.Prompt = "刷新授权已保存在私有恢复文件中，可以重试完成本地保存"
	}
	if credentialEditErr != nil {
		// The ended runner owns its business outcome. A later draft save may
		// block follow-up work, but cannot turn success/cancellation into a
		// different result or replace a security recovery diagnostic.
		job.RecoveryPaused, job.QueuedAt = true, time.Time{}
		job.Status = StatusFailed
		if job.OperationStatus == "succeeded" {
			job.Prompt = credentialEditErr.Error()
		}
	}
	event := m.applyEventLocked(JobEvent{JobID: job.ID, Status: job.Status, Stage: job.Stage, Prompt: job.Prompt, At: now, Data: map[string]any{"error": firstOperationErrorForLog(job)}})
	if job.LastError != "" {
		m.appendOperationLogLocked(job, JobEvent{JobID: job.ID, Stage: "credential_warning", At: now, Data: map[string]any{"mailApiError": job.LastError}})
	}
	m.persistLocked()
	if m.lastPersistError == nil {
		m.publishEventLocked(event)
	} else {
		// The observed operation result remains true. A failed local commit
		// must neither announce a durable completion nor replay remote work.
		appendCredentialWarning(job, "TASK_RESULT_PERSISTENCE: 本次操作结果已在内存保留，但元数据持久保存尚未确认；请修复存储，不要重复远端操作")
		if restart {
			// Credential cleanup may have succeeded, but the replacement
			// operation has not been reliably admitted. Never let the queue
			// replay authentication after this final metadata commit failed.
			m.removePendingLocked(job.ID)
			job.Status, job.OperationStatus, job.Stage = StatusFailed, "interrupted", "restart_blocked"
			job.RecoveryPaused, job.QueuedAt, job.FinishedAt = true, time.Time{}, &now
			job.Error, job.OperationError, job.Prompt = job.LastError, job.LastError, job.LastError
		}
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Cancellation cannot erase the observed outcome of a profile mutation that
// already ran. This closed display projection grants no input/state authority
// and is accepted only while the original runner still owns its callback.
func finalProfileObservation(e JobEvent, job *Job) (JobEvent, bool) {
	e = sanitizeEventForJob(SanitizeJobEvent(e), job)
	profile, ok := e.Data["profileCompletion"].(map[string]any)
	if !ok {
		return JobEvent{}, false
	}
	switch profile["outcome"] {
	case "not_sent", "rejected", "unknown":
		e.Stage = "profile_" + profile["outcome"].(string)
	case "succeeded":
		e.Stage = "profile_completed"
	default:
		return JobEvent{}, false
	}
	e.Status = ""
	e.Data = map[string]any{"profileCompletion": profile}
	return e, true
}
