package jobs

import (
	"errors"
	"regexp"
)

// TOTPIntentReceipt is a local request receipt. It is never an upstream
// idempotency key and never grants permission to repeat a remote mutation.
type TOTPIntentReceipt struct {
	ResetIntentID string `json:"resetIntentId"`
	OperationID   string `json:"operationId,omitempty"`
	Completed     bool   `json:"completed,omitempty"`
}

var totpIntentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

func ValidateTOTPIntent(values map[string]any) error {
	value, present := values["totpIntentId"]
	if !present {
		return nil
	}
	id, ok := value.(string)
	if !ok || !totpIntentPattern.MatchString(id) {
		return errors.New("2FA 请求标识无效")
	}
	return nil
}

// ReplayTOTPIntent is called before HTTP capability checks, so a delayed copy
// of an accepted request cannot start a second reset after the journal is gone.
func (m *Manager) ReplayTOTPIntent(id string, values map[string]any) (*Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return nil, false, osErrNotFound(id)
	}
	return m.replayTOTPIntentLocked(j, values)
}
func (m *Manager) replayTOTPIntentLocked(j *Job, values map[string]any) (*Job, bool, error) {
	if err := ValidateTOTPIntent(values); err != nil {
		return nil, false, err
	}
	id, _ := values["totpIntentId"].(string)
	receipt, ok := j.TOTPIntentReceipts[id]
	if id == "" || !ok {
		return nil, false, nil
	}
	if receipt.Completed || receipt.ResetIntentID != j.TOTPIntentID || (receipt.ResetIntentID == j.TOTPIntentID && (containsPending(m.pending, j.ID) || m.active[j.ID] != nil)) {
		return cloneJob(j), true, nil
	}
	return nil, false, nil
}
func recordTOTPIntent(j *Job, values map[string]any) {
	id, _ := values["totpIntentId"].(string)
	if id == "" {
		return
	}
	if j.TOTPIntentID == "" {
		j.TOTPIntentID = id
	}
	if j.TOTPIntentReceipts == nil {
		j.TOTPIntentReceipts = map[string]TOTPIntentReceipt{}
	}
	j.TOTPIntentReceipts[id] = TOTPIntentReceipt{ResetIntentID: j.TOTPIntentID}
}
