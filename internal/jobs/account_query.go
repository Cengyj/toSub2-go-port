package jobs

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrAccountQueryBusy         = errors.New("account query is blocked by another account operation")
	ErrAccountQuerySuperseded   = errors.New("account query no longer matches the current account")
	ErrAccountQueryProxyInvalid = errors.New("account query proxy configuration is invalid")
)

// AccountQueryLease reserves one normalized email without adding a job or
// changing its operation history. A mutating operation cancels the query and
// the scheduler waits for Release before starting its network work.
type AccountQueryLease struct {
	manager       *Manager
	job           *Job
	key           string
	ctx           context.Context
	cancel        context.CancelFunc
	stopRoot      func() bool
	once          sync.Once
	proxyURL      string
	proxyOverride bool
}

func (m *Manager) ReserveAccountQuery(ctx context.Context, id string) (*AccountQueryLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.closing || m.rootCtx.Err() != nil {
		return nil, ErrAccountQuerySuperseded
	}
	job, ok := m.jobs[id]
	if !ok {
		return nil, osErrNotFound(id)
	}
	key := emailKey(job.Email)
	if key == "" || m.deleting[key] || m.importing[key] || m.emailUse[key] != "" || m.accountQueries[key] != nil {
		return nil, ErrAccountQueryBusy
	}
	for _, candidate := range m.jobs {
		if emailKey(candidate.Email) == key && (m.active[candidate.ID] != nil || containsPending(m.pending, candidate.ID) || candidate.Status == StatusQueued) {
			return nil, ErrAccountQueryBusy
		}
	}
	if m.totpBlockedLocked(job.Email) {
		return nil, ErrTOTPRecoveryPending
	}
	proxyValue, proxyOverride := job.Input["proxyUrl"]
	proxyURL, validProxy := proxyValue.(string)
	if proxyOverride {
		if !validProxy {
			return nil, ErrAccountQueryProxyInvalid
		}
	}
	queryCtx, cancel := context.WithCancel(ctx)
	// Retain only the selected proxy plus sanitized account metadata. Query
	// ownership must not extend the lifetime of passwords, TOTP seeds or OTPs.
	lease := &AccountQueryLease{manager: m, job: SanitizeJobForPublic(job), key: key, ctx: queryCtx, cancel: cancel, proxyURL: proxyURL, proxyOverride: proxyOverride}
	lease.stopRoot = context.AfterFunc(m.rootCtx, cancel)
	if m.accountQueries == nil {
		m.accountQueries = make(map[string]*AccountQueryLease)
	}
	m.accountQueries[key] = lease
	m.queryWG.Add(1)
	return lease, nil
}

func (l *AccountQueryLease) Context() context.Context { return l.ctx }

// Job returns a public-safe snapshot, never authentication input.
func (l *AccountQueryLease) Job() *Job { return SanitizeJobForPublic(l.job) }

// ProxyInput returns only the process-local proxy override captured at admission.
// An explicit empty value means direct connection, not global proxy fallback.
func (l *AccountQueryLease) ProxyInput() (string, bool) {
	return l.proxyURL, l.proxyOverride
}

func (l *AccountQueryLease) Valid() bool {
	if l == nil || l.ctx.Err() != nil {
		return false
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.jobs[l.job.ID]
	if !exists {
		return false
	}
	currentProxy, currentHasProxy := current.Input["proxyUrl"]
	proxy, validProxy := currentProxy.(string)
	proxyMatches := currentHasProxy == l.proxyOverride && (!currentHasProxy || validProxy && proxy == l.proxyURL)
	return exists && !m.closing && m.rootCtx.Err() == nil && l.ctx.Err() == nil &&
		m.accountQueries[l.key] == l && !m.deleting[l.key] && !m.importing[l.key] &&
		current.Email == l.job.Email && current.CreatedAt.Equal(l.job.CreatedAt) &&
		current.Attempts == l.job.Attempts && current.AuthorizationMethod == l.job.AuthorizationMethod &&
		current.LastOperationAt.Equal(l.job.LastOperationAt) && current.UpdatedAt.Equal(l.job.UpdatedAt) &&
		current.ProxyConfigured == l.job.ProxyConfigured && current.ResultSaved == l.job.ResultSaved && proxyMatches
}

// Release is required after all network work and result validation finish.
// Cancellation alone does not release the gate while a caller is still using
// its grant; this prevents a login from overlapping an uncooperative request.
func (l *AccountQueryLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.cancel()
		l.stopRoot()
		m := l.manager
		m.mu.Lock()
		if m.accountQueries[l.key] == l {
			delete(m.accountQueries, l.key)
		}
		m.mu.Unlock()
		m.queryWG.Done()
		m.wakeScheduler()
	})
}

// Call while holding m.mu before changing account inputs or admitting work.
func (m *Manager) cancelAccountQueryLocked(email string) {
	if lease := m.accountQueries[emailKey(email)]; lease != nil {
		lease.cancel()
	}
}
