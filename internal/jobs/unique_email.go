package jobs

import (
	"errors"
	"reflect"
)

// SubmitUniqueEmail performs the legacy console's find-or-update operation in
// the same critical section as creation. Concurrent batch and single requests
// therefore produce one record for each normalized email address.
func (m *Manager) SubmitUniqueEmail(spec Spec) (job *Job, created bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.rootCtx.Err() != nil {
		return nil, false, errors.New("job manager is shutting down")
	}
	key := emailKey(spec.Email)
	if key == "" {
		return nil, false, errors.New("email is required")
	}
	if m.deleting[key] || m.importing[key] {
		return nil, false, errors.New("account is being deleted or restored")
	}
	var existing *Job
	for _, candidate := range m.jobs {
		if emailKey(candidate.Email) == key && (existing == nil || candidate.CreatedAt.After(existing.CreatedAt) || (candidate.CreatedAt.Equal(existing.CreatedAt) && candidate.ID > existing.ID)) {
			existing = candidate
		}
	}
	if existing != nil {
		if len(spec.Input) == 0 {
			return cloneJob(existing), false, nil
		}
		changed := false
		for _, key := range []string{"password", "totpSecret", "proxyUrl", "mailApiUrl", "mailRequestBody", "mailRequestMethod", "mailRequestHeaders"} {
			if value, present := spec.Input[key]; present && !reflect.DeepEqual(value, existing.Input[key]) {
				changed = true
			}
		}
		updated, err := m.updateLocked(existing.ID, spec.Input)
		if err == nil && changed {
			if cancel, active := m.active[existing.ID]; active {
				// Requeue only after the previous runner has released its browser,
				// transport and email lock; no two identities overlap.
				m.restart[existing.ID] = true
				cancel()
			}
		}
		return updated, false, err
	}
	job, err = m.submitLocked(spec)
	return job, err == nil, err
}
