package jobs

import (
	"errors"

	"tosub2-go-port/internal/store"
)

// ErrAdmissionPersistence means that the operation was not admitted. The
// caller may retry after repairing local storage; no new runner was started.
var ErrAdmissionPersistence = errors.New("任务变更保存失败，未提交执行；请检查数据目录后重试")

// ErrAdmissionUncertain is different from rejection: metadata was replaced
// before its directory sync failed. This process does not publish or run the
// changed operation. Startup may read that record but cannot execute its old intent.
var ErrAdmissionUncertain = errors.New("任务保存结果待核实；当前进程未启动新操作，重启后可能读到未确认的任务记录，但不会自动执行旧操作。请先检查任务状态再操作")

// ErrCancellationPersistence does not undo cancellation in this process. The
// previous state may still be on disk. Startup does not execute its old intent,
// but the user still needs an accurate cancellation result after storage repair.
var ErrCancellationPersistence = errors.New("当前进程已请求停止任务，但取消状态未能确认保存；重启后可能读到旧状态。请先修复存储并再次确认停止")

func admissionPersistenceError(err error) error {
	if errors.Is(err, store.ErrDurabilityUncertain) {
		return ErrAdmissionUncertain
	}
	return ErrAdmissionPersistence
}

// commitAdmissionLocked saves the complete new operation before making it
// executable or observable to subscribers. Callers must hold m.mu, capture
// before before changing the account, and leave pending untouched. A nil
// before denotes a new account that must disappear when its commit fails.
func (m *Manager) commitAdmissionLocked(job, before *Job, event JobEvent) error {
	if job.Status == StatusQueued {
		job.RecoveryPaused, job.AutoRepairSyncPaused = false, false
	}
	preparing, hadPreparation := m.inputPreparing[job.ID]
	event = m.applyEventLocked(event)
	m.persistLocked()
	if m.lastPersistError != nil {
		if before == nil {
			delete(m.jobs, job.ID)
		} else {
			// Preserve the pointer held by an existing queue entry or input
			// reservation, as well as its old authorization and local inputs.
			*job = *before
		}
		if hadPreparation {
			m.inputPreparing[job.ID] = preparing
		}
		return admissionPersistenceError(m.lastPersistError)
	}
	if job.Status == StatusQueued && !containsPending(m.pending, job.ID) {
		m.pending = append(m.pending, job)
	}
	m.cancelAccountQueryLocked(job.Email)
	m.publishEventLocked(event)
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}
