package jobs

import (
	"errors"
	"time"
)

// QueueAutoRepair binds the pending remote IDs to the login operation under
// the scheduler lock. A user action cannot start the job between these writes.
func (m *Manager) QueueAutoRepair(id string, accountIDs []int, backend string) (*Job, error) {
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
	job.ResetTOTP = false
	initializeTOTPWorkflow(job, nil)
	job.LastOperationType = "automatic_relogin"
	job.LastOperationAt = now
	job.Status = StatusQueued
	job.QueuedAt = now
	job.UpdatedAt = now
	job.FinishedAt = nil
	job.Error = ""
	if err := m.commitAdmissionLocked(job, before, JobEvent{JobID: job.ID, Status: StatusQueued, Stage: "queued", Prompt: "等待自动重新登录", At: now}); err != nil {
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
