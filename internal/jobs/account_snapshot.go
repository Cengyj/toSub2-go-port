package jobs

import "sort"

// AccountSnapshot is a read-only account-list projection. Availability and
// queue position are captured with the account under one lock; no credential,
// recovery file, or network request is read to produce it. Job contains no
// input, temporary enrollment material, or historical log tail.
type AccountSnapshot struct {
	Job               *Job
	PasswordAvailable bool
	TOTPAvailable     bool
	AutoEmailOTP      bool
	QueuePosition     int
}

// AccountStatistics counts the latest record for each normalized email.
// Available retains the existing saved-result display contract; it does not
// replace the live authorization and recovery checks before an operation.
type AccountStatistics struct {
	Active, Queued, Completed, Available int
}

// AccountListSnapshot keeps global counts and the matched account views at
// the same manager state. It carries no HTTP pagination or execution authority.
type AccountListSnapshot struct {
	Accounts []AccountSnapshot
	Total    int
	Stats    AccountStatistics
}

// PublicAccounts selects the newest record per normalized email without
// cloning historical records. History remains available through List/Get and
// the explicit log API. Its ordering matches the console's existing contract:
// newest creation first, with descending ID as the deterministic tie breaker.
func (m *Manager) PublicAccounts() []AccountSnapshot {
	return m.PublicAccountsFiltered(nil).Accounts
}

// PublicAccountsFiltered builds full public views only for matching emails.
// Empty or whitespace-only filters keep the existing all-accounts contract.
// Counts include all latest accounts, independently of the requested filter.
func (m *Manager) PublicAccountsFiltered(emails []string) AccountListSnapshot {
	wanted := make(map[string]bool, len(emails))
	for _, email := range emails {
		if key := emailKey(email); key != "" {
			wanted[key] = true
		}
	}
	m.mu.Lock()
	latest := make(map[string]*Job, len(m.jobs))
	for _, job := range m.jobs {
		key := emailKey(job.Email)
		previous := latest[key]
		if previous == nil || job.CreatedAt.After(previous.CreatedAt) || job.CreatedAt.Equal(previous.CreatedAt) && job.ID > previous.ID {
			latest[key] = job
		}
	}
	positions := make(map[string]int, len(m.pending))
	for i, job := range m.pending {
		if job.Status == StatusQueued {
			positions[job.ID] = i + 1
		}
	}
	capacity := len(latest)
	if len(wanted) > 0 {
		capacity = min(capacity, len(wanted))
	}
	result := AccountListSnapshot{Accounts: make([]AccountSnapshot, 0, capacity), Total: len(latest)}
	knownSecrets := make([][]string, 0, capacity)
	for email, job := range latest {
		if job.ResultSaved && !job.TOTPRecoveryPending && !job.TOTPCredentialInvalidated {
			result.Stats.Available++
		}
		switch {
		case IsActiveStatus(job.Status):
			result.Stats.Active++
		case job.Status == StatusQueued:
			result.Stats.Queued++
		case job.Status == StatusSucceeded:
			result.Stats.Completed++
		}
		if len(wanted) > 0 && !wanted[email] {
			continue
		}
		password, _ := job.Input["password"].(string)
		totp, _ := job.Input["totpSecret"].(string)
		view := *job
		view.Logs, view.LogEntries = nil, nil
		// Capture the complete redaction context before discarding private
		// input. Every mutable public field is detached while holding the lock.
		knownSecrets = append(knownSecrets, jobKnownSecrets(job))
		result.Accounts = append(result.Accounts, AccountSnapshot{
			Job:               clonePublicJob(&view),
			PasswordAvailable: password != "",
			TOTPAvailable:     totp != "",
			AutoEmailOTP:      job.MailAPIURL != "",
			QueuePosition:     positions[job.ID],
		})
	}
	m.mu.Unlock()
	for i, account := range result.Accounts {
		sanitizePublicJobSnapshot(account.Job, knownSecrets[i], nil)
	}
	sort.Slice(result.Accounts, func(i, j int) bool {
		if result.Accounts[i].Job.CreatedAt.Equal(result.Accounts[j].Job.CreatedAt) {
			return result.Accounts[i].Job.ID > result.Accounts[j].Job.ID
		}
		return result.Accounts[i].Job.CreatedAt.After(result.Accounts[j].Job.CreatedAt)
	})
	return result
}
