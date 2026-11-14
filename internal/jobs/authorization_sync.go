package jobs

import "context"

// SetAuthorizationSyncCheck installs an application-owned preflight boundary.
// It runs outside the manager lock and inside the caller's workflow ownership.
func (m *Manager) SetAuthorizationSyncCheck(check func(context.Context, string, map[string]any) error) {
	m.mu.Lock()
	m.authorizationSyncCheck = check
	m.mu.Unlock()
}

func (m *Manager) CheckAuthorizationSync(ctx context.Context, id string, account map[string]any) (bool, error) {
	m.mu.Lock()
	check := m.authorizationSyncCheck
	m.mu.Unlock()
	if check == nil {
		return false, nil
	}
	return true, check(ctx, id, account)
}

// ActiveAccountOperation is a read-only ownership check for application code
// already running in the scheduler. It does not admit or release an operation.
func (m *Manager) ActiveAccountOperation(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closing && m.rootCtx.Err() == nil && m.active[id] != nil
}
