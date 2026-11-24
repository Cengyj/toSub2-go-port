package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestQueryAccountsPreservesHistoryVersionAndExecutionSnapshot(t *testing.T) {
	m := NewManager(t.Context(), 2, nil, nil)
	defer m.cancel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	older := &Job{ID: "older", Email: "same@example.test", Status: StatusSucceeded, CreatedAt: at.Add(-time.Hour), ResultSaved: true}
	current := &Job{ID: "a", Email: "same@example.test", Status: StatusRunning, CreatedAt: at, UpdatedAt: at.Add(time.Second), LastOperationAt: at.Add(time.Minute), Attempts: 7, AuthorizationMethod: "bps", ProxyConfigured: true, TOTPRecoveryPending: true,
		Input: map[string]any{"password": "private-query-canary"}, Prompt: "private-query-canary", Logs: []string{"private-query-canary"}, TOTPSetupSecret: "private-query-canary"}
	m.jobs[older.ID], m.jobs[current.ID] = older, current
	m.jobs["b"] = &Job{ID: "b", Email: "other@example.test", CreatedAt: at, TOTPCredentialInvalidated: true}
	m.active[current.ID] = &executionAttempt{}
	m.metadataWriter = func(context.Context, string, any) error {
		t.Error("read-only inventory persisted metadata")
		return nil
	}
	got := m.QueryAccounts()
	if len(got) != 3 || got[0].ID != "older" || got[1].ID != "a" || got[2].ID != "b" {
		t.Fatal("inventory collapsed retained history or changed creation/tie order")
	}
	if !got[0].ResultSaved || !got[1].Executing || !got[1].RecoveryBlocked || !got[2].RecoveryBlocked || got[1].Attempts != 7 || got[1].AuthorizationMethod != "bps" || !got[1].ProxyConfigured || got[1].UpdatedAt != current.UpdatedAt || got[1].LastOperationAt != current.LastOperationAt {
		t.Fatal("inventory lost an authorization, recovery, execution, or version fence")
	}
	raw, err := json.Marshal(got)
	if err != nil || strings.Contains(string(raw), "private-query-canary") {
		t.Fatal("inventory copied private input or historical diagnostics", err)
	}
	m.mu.Lock()
	current.Attempts, current.AuthorizationMethod, current.TOTPRecoveryPending = 8, "chatgpt", false
	current.UpdatedAt = current.UpdatedAt.Add(time.Hour)
	delete(m.active, current.ID)
	m.mu.Unlock()
	next := m.QueryAccounts()
	if got[1].Attempts != 7 || !got[1].Executing || got[1].AuthorizationMethod != "bps" || !got[1].RecoveryBlocked || next[1].Attempts != 8 || next[1].Executing || next[1].RecoveryBlocked || next[1].UpdatedAt.Equal(got[1].UpdatedAt) {
		t.Fatal("inventory returned a stale cached view or shared live mutable state")
	}
	m.mu.Lock()
	m.active[current.ID] = &executionAttempt{}
	m.closing = true
	m.mu.Unlock()
	if m.QueryAccounts()[1].Executing {
		t.Fatal("closing manager advertised a live execution")
	}
	m.mu.Lock()
	m.closing = false
	m.mu.Unlock()
	m.cancel()
	if m.QueryAccounts()[1].Executing {
		t.Fatal("canceled manager advertised a live execution")
	}
}
