package jobs

import "time"

// TOTPListState is a read-only presentation result. It grants no execution or
// export authority; commands still run their live credential and journal gates.
type TOTPListState struct {
	CompletionPending bool
	ExportPending     bool
	CompletionError   error
	// OperationInFlight means a same-email setup, reset or local completion
	// still owns a queue entry or runner. It is independent of recovery evidence:
	// normal in-flight work is isolated too, and cancellation retains runner
	// ownership until cleanup returns. A saved status alone proves no owner.
	OperationInFlight bool
	// OwnOperationAt identifies the existing presentation generation only
	// when this row owns a TOTP operation. Peers and retained status fields
	// cannot supply it. This timestamp grants no execution authority.
	OwnOperationAt time.Time
}

func (m *Manager) totpRelatedJobsLocked(email string) []*Job {
	key := emailKey(email)
	var related []*Job
	for _, job := range m.jobs {
		if emailKey(job.Email) == key {
			related = append(related, job)
		}
	}
	return related
}

// TOTPListStates groups all retained history once. Each selected account's
// actual private journal is inspected on every call; there is no persistent
// cache that could hide new cross-process recovery evidence. Healthy lists
// never open the credential store. Valid completions use the existing exact
// completion gate, restricted to the already collected account history.
func (m *Manager) TOTPListStates(ids []string) map[string]TOTPListState {
	m.mu.Lock()
	defer m.mu.Unlock()
	wantedEmails := make(map[string]bool, len(ids))
	for _, id := range ids {
		if job := m.jobs[id]; job != nil {
			wantedEmails[emailKey(job.Email)] = true
		}
	}
	groups := make(map[string][]*Job, len(wantedEmails))
	for _, job := range m.jobs {
		if key := emailKey(job.Email); wantedEmails[key] {
			groups[key] = append(groups[key], job)
		}
	}
	pendingIDs := make(map[string]bool, len(m.pending))
	for _, job := range m.pending {
		pendingIDs[job.ID] = true
	}
	completion := make(map[string]bool, len(ids))
	blocked := make(map[string]bool, len(groups))
	inFlight := make(map[string]bool, len(groups))
	for email, related := range groups {
		for _, job := range related {
			pending := m.totpResetCompletionPendingLocked(job)
			completion[job.ID] = pending
			owned := pendingIDs[job.ID] || m.active[job.ID] != nil
			if totpOperation(job.Operation) && owned {
				inFlight[email] = true
			}
			// Setup ownership explains an existing journal gate, but does not
			// introduce the reset-only export gate before such evidence exists.
			if pending || job.TOTPRecoveryPending || job.TOTPCredentialInvalidated || resetOperation(job.Operation) && owned {
				blocked[email] = true
			}
		}
		if !blocked[email] {
			blocked[email] = m.unresolvedTOTPJournalForJobsLocked(email, related)
		}
	}
	states := make(map[string]TOTPListState, len(ids))
	for _, id := range ids {
		job := m.jobs[id]
		if job == nil {
			continue
		}
		key := emailKey(job.Email)
		state := TOTPListState{CompletionPending: completion[id], ExportPending: blocked[key], OperationInFlight: inFlight[key]}
		if totpOperation(job.Operation) && (pendingIDs[id] || m.active[id] != nil) {
			state.OwnOperationAt = job.LastOperationAt
		}
		if state.CompletionPending {
			state.CompletionError = m.canCompleteTOTPResetForJobsLocked(id, true, groups[key], pendingIDs)
		}
		states[id] = state
	}
	return states
}
