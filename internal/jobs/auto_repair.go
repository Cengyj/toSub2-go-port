package jobs

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
)

// AutoRepairPolicy is the current application policy at the execution boundary.
// The console owns configuration; jobs only needs these non-secret decisions.
type AutoRepairPolicy struct {
	Enabled           bool
	Backend           string
	ExcludedPlanTypes []string
}

// SetAutoRepairPolicy replaces the execution policy with an owned copy. An
// unset policy preserves standalone Manager use; applications must set it
// before starting the scheduler, including when monitoring is disabled.
func (m *Manager) SetAutoRepairPolicy(policy AutoRepairPolicy) {
	copy := AutoRepairPolicy{Enabled: policy.Enabled, Backend: strings.TrimSpace(policy.Backend)}
	seen := map[string]bool{}
	for _, value := range policy.ExcludedPlanTypes {
		plan := accountplan.Normalize(value)
		if plan == "unknown" && !strings.EqualFold(strings.TrimSpace(value), "unknown") {
			continue
		}
		if !seen[plan] {
			copy.ExcludedPlanTypes = append(copy.ExcludedPlanTypes, plan)
			seen[plan] = true
		}
	}
	m.mu.Lock()
	m.autoRepairPolicy = &copy
	m.mu.Unlock()
	m.wakeScheduler()
}

func (m *Manager) wakeScheduler() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func isAutomaticRepair(job *Job) bool {
	return job != nil && job.Operation == "relogin" && job.LastOperationType == "automatic_relogin"
}

func (m *Manager) autoRepairSkipReasonLocked(job *Job) string {
	if !isAutomaticRepair(job) || m.autoRepairPolicy == nil {
		return ""
	}
	policy := m.autoRepairPolicy
	if !policy.Enabled {
		return "自动巡检已关闭，已跳过排队的自动重登"
	}
	if policy.Backend == "" || policy.Backend != job.AutoRepairBackend {
		return "Sub2API 后端已变更或不可用，已跳过排队的自动重登"
	}
	if accountplan.AutomaticRepairExcluded(job.PlanType, job.AutoRepairQueuedRemotePlanTypes, policy.ExcludedPlanTypes) {
		return "当前本地或远端套餐已列入排除范围，已跳过自动重登"
	}
	return ""
}

func clearAutoRepairTargets(job *Job) {
	job.AutoRepairPendingAccountIDs = nil
	job.AutoRepairBackend = ""
	job.AutoRepairLastError = ""
}

func normalizedRepairPlans(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		plan := accountplan.Normalize(value)
		if !seen[plan] {
			out = append(out, plan)
			seen[plan] = true
		}
	}
	return out
}

func setAutoRepairSkipped(job *Job, reason string, now time.Time) {
	job.Status, job.OperationStatus = StatusCanceled, "skipped"
	job.Stage, job.Prompt = "auto_repair_skipped", reason
	job.AutoRepairSkippedReason = reason
	job.Error, job.OperationError = "", ""
	job.StartedAt, job.FinishedAt, job.UpdatedAt = nil, &now, now
	clearAutoRepairTargets(job)
}

// A denied execution remains denied even when saving its outcome fails. Keep
// the in-memory skip, remove the runnable entry, and expose the storage warning.
// Restore also rejects old queued automatic intents, so a pre-replacement
// failure cannot replay this queue silently after a restart.
func (m *Manager) skipAutoRepairLocked(job *Job, reason string) {
	now := time.Now().UTC()
	setAutoRepairSkipped(job, reason, now)
	event := m.applyEventLocked(JobEvent{JobID: job.ID, Status: job.Status, Stage: job.Stage, Prompt: job.Prompt, At: now})
	m.persistLocked()
	if m.lastPersistError != nil {
		job.OperationError = "自动重登已阻止，但跳过状态未确认保存；请检查数据目录后刷新核实"
		job.Error = job.OperationError
		job.Prompt = reason + "；" + job.OperationError
		event.Prompt = job.Prompt
		m.appendOperationLogLocked(job, JobEvent{JobID: job.ID, Stage: "auto_repair_skip_save_failed", At: now, Data: map[string]any{"error": job.OperationError}})
	}
	m.publishEventLocked(event)
}

// QueueAutoRepair binds the pending remote IDs to the login operation under
// the scheduler lock. A user action cannot start the job between these writes.
func (m *Manager) QueueAutoRepair(id string, accountIDs []int, backend string, remotePlans ...string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.rootCtx.Err() != nil {
		return nil, errors.New("job manager is shutting down")
	}
	job, ok := m.jobs[id]
	if !ok {
		return nil, osErrNotFound(id)
	}
	if err := m.checkTOTPActionLocked(job.Email); err != nil {
		return nil, err
	}
	if m.deleting[emailKey(job.Email)] || m.importing[emailKey(job.Email)] {
		return nil, errors.New("account is being deleted or restored")
	}
	if _, active := m.active[id]; active || job.Status == StatusQueued || containsPending(m.pending, id) {
		return nil, errors.New("job is already queued or running")
	}
	if job.AutoRepairBlocked {
		return nil, errors.New("account is permanently unavailable")
	}
	ids := []int{}
	seen := map[int]bool{}
	for _, id := range accountIDs {
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || backend == "" {
		return nil, errors.New("auto-repair target is empty")
	}
	before := cloneJob(job)
	now := time.Now().UTC()
	job.AutoRepairPendingAccountIDs = ids
	job.AutoRepairBackend = backend
	job.AutoRepairLastAttemptAt = &now
	job.AutoRepairLastError = ""
	job.Operation = "relogin"
	beginOperationState(job)
	job.AutoRepairQueuedPlanType = accountplan.Normalize(job.PlanType)
	job.AutoRepairQueuedRemotePlanTypes = normalizedRepairPlans(remotePlans)
	job.ResetTOTP = false
	initializeTOTPWorkflow(job, nil)
	job.LastOperationType = "automatic_relogin"
	job.LastOperationAt = now
	job.Status = StatusQueued
	job.QueuedAt = now
	job.UpdatedAt = now
	job.FinishedAt = nil
	job.Error = ""
	prompt := "等待自动重新登录（入队本地套餐：" + job.AutoRepairQueuedPlanType
	if len(job.AutoRepairQueuedRemotePlanTypes) > 0 {
		prompt += "；远端套餐：" + strings.Join(job.AutoRepairQueuedRemotePlanTypes, "、")
	}
	prompt += "）"
	if err := m.commitAdmissionLocked(job, before, JobEvent{JobID: job.ID, Status: StatusQueued, Stage: "queued", Prompt: prompt, At: now}); err != nil {
		return nil, err
	}
	return cloneJob(job), nil
}

// ErrAutoRepairSuperseded means that a user action or newer account operation
// replaced the authorization whose remote update was in flight.
var ErrAutoRepairSuperseded = errors.New("auto-repair operation was superseded")

// CheckAutoRepair prevents the next remote update step from using an obsolete
// authorization snapshot. CommitAutoRepair repeats the check under the same
// lock as its mutation, since a check before a network request is not atomic.
func (m *Manager) CheckAutoRepair(expected *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.currentAutoRepairLocked(expected)
	return err
}

func (m *Manager) currentAutoRepairLocked(expected *Job) (*Job, error) {
	if expected == nil {
		return nil, ErrAutoRepairSuperseded
	}
	job, ok := m.jobs[expected.ID]
	if !ok || job.Status != StatusSucceeded || !job.ResultSaved || job.AutoRepairBlocked || m.deleting[emailKey(job.Email)] {
		return nil, ErrAutoRepairSuperseded
	}
	if m.totpBlockedLocked(job.Email) {
		return nil, ErrAutoRepairSuperseded
	}
	if _, active := m.active[job.ID]; active {
		return nil, ErrAutoRepairSuperseded
	}
	if job.Email != expected.Email || job.Attempts != expected.Attempts || job.Operation != expected.Operation || !job.LastOperationAt.Equal(expected.LastOperationAt) || job.AutoRepairBackend != expected.AutoRepairBackend || job.AutoRepairBackend == "" || len(job.AutoRepairPendingAccountIDs) == 0 || len(job.AutoRepairPendingAccountIDs) != len(expected.AutoRepairPendingAccountIDs) || !sameRepairTime(job.AutoRepairLastAttemptAt, expected.AutoRepairLastAttemptAt) || !sameRepairTime(job.AutoRepairLastSuccessAt, expected.AutoRepairLastSuccessAt) {
		return nil, ErrAutoRepairSuperseded
	}
	for i, id := range job.AutoRepairPendingAccountIDs {
		if id != expected.AutoRepairPendingAccountIDs[i] {
			return nil, ErrAutoRepairSuperseded
		}
	}
	for _, candidate := range m.jobs {
		if emailKey(candidate.Email) == emailKey(job.Email) && (candidate.CreatedAt.After(job.CreatedAt) || (candidate.CreatedAt.Equal(job.CreatedAt) && candidate.ID > job.ID)) {
			return nil, ErrAutoRepairSuperseded
		}
	}
	return job, nil
}

func sameRepairTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// PauseAutoRepairTransfer records a policy pause without discarding the saved
// authorization or pending remote IDs. It never admits or performs a transfer.
func (m *Manager) PauseAutoRepairTransfer(expected *Job, reason string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.currentAutoRepairLocked(expected)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errors.New("auto-repair pause reason is required")
	}
	reason = safeError(errors.New(reason))
	if job.AutoRepairSkippedReason == reason && m.lastPersistError == nil {
		return cloneJob(job), nil
	}
	before := cloneJob(job)
	now := time.Now().UTC()
	job.AutoRepairSkippedReason, job.UpdatedAt = reason, now
	event := SanitizeJobEvent(JobEvent{JobID: job.ID, Stage: "auto_repair_upload_paused", Status: job.Status, Prompt: reason, At: now})
	m.appendOperationLogLocked(job, event)
	m.persistLocked()
	if m.lastPersistError != nil {
		*job = *before
		return nil, fmt.Errorf("auto-repair pause metadata could not be saved: %w", admissionPersistenceError(m.lastPersistError))
	}
	m.publishEventLocked(event)
	return cloneJob(job), nil
}

// CommitAutoRepair records a partial or complete transfer only while its
// original authorization and target list are still current. The returned
// snapshot must be used for any subsequent transfer step.
func (m *Manager) CommitAutoRepair(expected *Job, pending []int, lastError string, success bool) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.currentAutoRepairLocked(expected)
	if err != nil {
		return nil, err
	}
	if success != (len(pending) == 0) || (success && lastError != "") {
		return nil, errors.New("invalid auto-repair transfer result")
	}
	valid := make(map[int]bool, len(job.AutoRepairPendingAccountIDs))
	for _, id := range job.AutoRepairPendingAccountIDs {
		valid[id] = true
	}
	for _, id := range pending {
		if !valid[id] {
			return nil, errors.New("auto-repair transfer introduced an unrelated target")
		}
		delete(valid, id)
	}
	before := cloneJob(job)
	now := time.Now().UTC()
	job.AutoRepairPendingAccountIDs = append([]int(nil), pending...)
	job.AutoRepairLastError = ""
	job.AutoRepairSkippedReason = ""
	if lastError != "" {
		job.AutoRepairLastError = safeError(errors.New(lastError))
	}
	job.AutoRepairLastAttemptAt = &now
	job.UpdatedAt = now
	if success {
		job.AutoRepairBackend = ""
		job.AutoRepairLastSuccessAt = &now
		job.AutoRepairBlockedReason = ""
	}
	m.persistLocked()
	if m.lastPersistError != nil {
		m.jobs[job.ID] = before
		return nil, errors.New("auto-repair transfer metadata could not be saved")
	}
	return cloneJob(job), nil
}
