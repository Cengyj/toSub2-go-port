package jobs

import (
	"sort"
	"time"
)

// QueryAccount contains only the account identity/version and authorization
// eligibility needed by read-only services. It never copies credential input,
// prompts, diagnostics, logs, grants or temporary enrollment material.
type QueryAccount struct {
	ID, Email, Status, AuthorizationMethod                   string
	CreatedAt, UpdatedAt, LastOperationAt                    time.Time
	Attempts                                                 int
	ResultSaved, ProxyConfigured, RecoveryBlocked, Executing bool
}

func (m *Manager) QueryAccounts() []QueryAccount {
	m.mu.Lock()
	out := make([]QueryAccount, 0, len(m.jobs))
	running := !m.closing && m.rootCtx.Err() == nil
	for _, j := range m.jobs {
		out = append(out, QueryAccount{ID: j.ID, Email: j.Email, Status: j.Status, AuthorizationMethod: j.AuthorizationMethod,
			CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, LastOperationAt: j.LastOperationAt, Attempts: j.Attempts,
			ResultSaved: j.ResultSaved, ProxyConfigured: j.ProxyConfigured, RecoveryBlocked: j.TOTPRecoveryPending || j.TOTPCredentialInvalidated,
			Executing: running && m.active[j.ID] != nil})
	}
	m.mu.Unlock()
	// This view contains only copied scalar values. Sorting cannot observe
	// or delay later account changes once the consistent snapshot is captured.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}
