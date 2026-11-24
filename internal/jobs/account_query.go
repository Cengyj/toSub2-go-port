package jobs

import (
	"context"
	"errors"
	"sync"

	"tosub2-go-port/internal/accountlock"
)

var (
	ErrAccountQueryBusy            = errors.New("account query is blocked by another account operation")
	ErrAccountQuerySuperseded      = errors.New("account query no longer matches the current account")
	ErrAccountQueryLockUnavailable = errors.New("account query cross-process ownership is unavailable")
	ErrAccountQueryProxyInvalid    = errors.New("account query proxy configuration is invalid")
)

// AccountQueryLease reserves one normalized email without adding a job or
// changing its operation history. A mutating operation cancels the query and
// the scheduler waits for Release before starting its network work.
type AccountQueryLease struct {
	manager            *Manager
	job                *Job
	key                string
	ctx                context.Context
	cancel             context.CancelFunc
	stopRoot           func() bool
	once               sync.Once
	proxyURL           string
	proxyOverride      bool
	releaseAccountLock func() error
	accountLockMu      sync.Mutex
}

// ReserveAccountQuery holds the same host-wide account lock as login and
// security operations. OS acquisition occurs outside m.mu; the local reservation
// remains in place during acquisition and until OS cleanup has completed.
func (m *Manager) ReserveAccountQuery(ctx context.Context, id string) (*AccountQueryLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	acquire := m.accountOperationLease
	lease, err := m.reserveAccountQueryLocked(ctx, id)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if acquire == nil {
		acquire = func(email string) (func() error, error) { return accountlock.Acquire("https://chatgpt.com", email) }
	}
	release, err := acquire(lease.job.Email)
	if err != nil || release == nil {
		if release != nil {
			_ = release()
		}
		lease.Release()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if errors.Is(err, accountlock.ErrBusy) {
			return nil, ErrAccountQueryBusy
		}
		return nil, ErrAccountQueryLockUnavailable
	}
	lease.accountLockMu.Lock()
	lease.releaseAccountLock = release
	lease.accountLockMu.Unlock()
	// A different process may have published a recovery journal between the
	// preliminary eligibility check and OS acquisition. Recheck while holding
	// both ownership layers; admission must not borrow a pre-operation verdict.
	m.mu.Lock()
	valid := lease.validLocked()
	recoveryPending := valid && (m.totpBlockedLocked(lease.job.Email) || m.unresolvedTOTPJournalLocked(lease.job.Email))
	m.mu.Unlock()
	if !valid || recoveryPending {
		lease.Release()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if recoveryPending {
			return nil, ErrTOTPRecoveryPending
		}
		return nil, ErrAccountQuerySuperseded
	}
	return lease, nil
}

func (m *Manager) reserveAccountQueryLocked(ctx context.Context, id string) (*AccountQueryLease, error) {
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
	if m.totpBlockedLocked(job.Email) || m.unresolvedTOTPJournalLocked(job.Email) {
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
	return l.validLocked()
}

// validLocked is shared with terminal query commits to avoid a check/write race.
func (l *AccountQueryLease) validLocked() bool {
	m := l.manager
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
		l.accountLockMu.Lock()
		if l.releaseAccountLock != nil {
			_ = l.releaseAccountLock()
			l.releaseAccountLock = nil
		}
		l.accountLockMu.Unlock()
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

// A local credential edit invalidates this read-only query, but must retain
// its OS ownership until the edit's complete write/readback finishes. Never
// wait for acquisition or cleanup while the manager mutex is held.
func (l *AccountQueryLease) tryBorrowAccountLock() func() error {
	if !l.accountLockMu.TryLock() {
		return nil
	}
	if l.releaseAccountLock == nil {
		l.accountLockMu.Unlock()
		return nil
	}
	return func() error { l.accountLockMu.Unlock(); return nil }
}

// Call while holding m.mu before changing account inputs or admitting work.
func (m *Manager) cancelAccountQueryLocked(email string) {
	if lease := m.accountQueries[emailKey(email)]; lease != nil {
		lease.cancel()
	}
}
