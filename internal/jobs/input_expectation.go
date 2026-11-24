package jobs

import "time"

// InputExpectation binds a response to the exact operation and prompt shown
// to its operator. A reused job ID does not grant authority over a later run.
type InputExpectation struct {
	OperationAt time.Time `json:"operationAt"`
	Stage       string    `json:"stage"`
}

func (m *Manager) matchesInputExpectationLocked(id string, expected InputExpectation) bool {
	job := m.jobs[id]
	return job != nil && !expected.OperationAt.IsZero() && expected.Stage != "" &&
		job.LastOperationAt.Equal(expected.OperationAt) && job.Status == expected.Stage &&
		!m.active[id].canceled()
}

// InputForStageExpected checks the displayed context and commits its input in
// one critical section, including compatibility requests without an action.
func (m *Manager) InputForStageExpected(id string, expected InputExpectation, values map[string]any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.matchesInputExpectationLocked(id, expected) {
		return false
	}
	return m.inputLocked(id, expected.Stage, values)
}

func (m *Manager) InputForStageWithPreparationExpected(id string, expected InputExpectation, values map[string]any, prepare func()) bool {
	return m.inputForStageWithPreparation(id, expected.Stage, &expected, values, prepare)
}
