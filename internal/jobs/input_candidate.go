package jobs

// DiscardInputIfEqual removes only the process-local value already supplied to
// an automatic protocol attempt. Matching and removal share the manager lock so
// a newer manual value cannot be discarded between a read and a later delete.
// Protected credentials and their availability flags are left intact.
func (m *Manager) DiscardInputIfEqual(id, key, expected string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return false
	}
	value, ok := j.Input[key].(string)
	if !ok || value != expected {
		return false
	}
	delete(j.Input, key)
	return true
}
