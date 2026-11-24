package jobs

import "errors"

var ErrAdmissionPaused = errors.New("MAINTENANCE_ADMISSION_PAUSED: 维护模式已暂停新任务，已受理任务仍可完成输入、取消和保存结果")

// ActivitySnapshot describes work owned by the scheduler. The application
// combines it with its upload/query/background-service leases before declaring
// maintenance drained; this alone is not a global idle check.
type ActivitySnapshot struct {
	AdmissionPaused bool `json:"admissionPaused"`
	Queued          int  `json:"queued"`
	Running         int  `json:"running"`
	Queries         int  `json:"queries"`
	AccountChanges  int  `json:"accountChanges"`
	Drained         bool `json:"drained"`
}

// SetAdmissionPaused applies the application-owned maintenance policy. It does
// not cancel work, mutate persisted operation outcomes, or start old intents.
func (m *Manager) SetAdmissionPaused(paused bool) {
	m.mu.Lock()
	m.admissionPaused = paused
	m.mu.Unlock()
}

func (m *Manager) ActivitySnapshot() ActivitySnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := ActivitySnapshot{AdmissionPaused: m.admissionPaused, Queued: len(m.pending), Running: len(m.active), Queries: len(m.accountQueries), AccountChanges: len(m.importing) + len(m.deleting)}
	out.Drained = out.Queued == 0 && out.Running == 0 && out.Queries == 0 && out.AccountChanges == 0
	return out
}
