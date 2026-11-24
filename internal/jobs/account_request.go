package jobs

import (
	"errors"
	"sort"
	"time"
)

// AccountRequestObservation is display-only metadata, never a recovery or retry
// decision. HTTPStatus is an observed response status, not a derived category.
type AccountRequestObservation struct {
	Source     string    `json:"source"`
	Status     string    `json:"status"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
	ErrorCode  string    `json:"errorCode,omitempty"`
	Message    string    `json:"message,omitempty"`
	FinishedAt time.Time `json:"finishedAt"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	Historical bool      `json:"historical,omitempty"`
}

var ErrAccountRequestSuperseded = errors.New("account request observation superseded")

type AccountRequestRecorder struct {
	manager                           *Manager
	job                               *Job
	source, authorizationMethod       string
	createdAt, operationAt, startedAt time.Time
	attempts                          int
	completed                         bool // guarded by manager.mu
	queryLease                        *AccountQueryLease
}

func accountRequestSource(source string) bool {
	switch source {
	case "subscription", "models", "candy", "pelican", "authentication", "refresh", "totp", "password", "logout", "usage", "bps", "resetCredits":
		return true
	}
	return false
}

// BeginAccountRequest does not mutate the job or invalidate query leases.
func (m *Manager) BeginAccountRequest(id, source string) *AccountRequestRecorder {
	if m == nil || !accountRequestSource(source) {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil || m.closing || m.deleting[emailKey(j.Email)] || m.importing[emailKey(j.Email)] {
		return nil
	}
	startedAt := time.Now().UTC()
	// Restore can carry a newer timestamp after a system-clock correction.
	// Advancing the logical order keeps a new request able to replace it.
	for _, old := range j.AccountRequestObservations {
		if old.Source == source && old.StartedAt.After(m.accountRequestClock) {
			m.accountRequestClock = old.StartedAt
		}
	}
	// Concurrent starts can share a wall-clock tick. Keep a strict local order
	// without touching the account's revision or scheduling another operation.
	if !startedAt.After(m.accountRequestClock) {
		startedAt = m.accountRequestClock.Add(time.Nanosecond)
	}
	m.accountRequestClock = startedAt
	r := &AccountRequestRecorder{manager: m, job: j, source: source, authorizationMethod: j.AuthorizationMethod, createdAt: j.CreatedAt, operationAt: j.LastOperationAt, attempts: j.Attempts, startedAt: startedAt}
	if lease := m.accountQueries[emailKey(j.Email)]; lease != nil && lease.job.ID == id {
		r.queryLease = lease
	}
	return r
}

// Complete replaces only this source's latest batch. A successful batch is
// retained as a tombstone so old legacy snapshots cannot revive an error.
// Persistence is best-effort: callers must not retry an upstream mutation on
// this error. The latest in-memory diagnostic remains available if disk fails.
func (r *AccountRequestRecorder) Complete(observations ...AccountRequestObservation) error {
	if r == nil || len(observations) == 0 {
		return nil
	}
	m := r.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[r.job.ID]
	if r.completed || j != r.job || m.closing || m.deleting[emailKey(j.Email)] || m.importing[emailKey(j.Email)] || !j.CreatedAt.Equal(r.createdAt) || j.AuthorizationMethod != r.authorizationMethod || j.Attempts != r.attempts || !j.LastOperationAt.Equal(r.operationAt) {
		return ErrAccountRequestSuperseded
	}
	for _, old := range j.AccountRequestObservations {
		if old.Source == r.source && old.StartedAt.After(r.startedAt) {
			return ErrAccountRequestSuperseded
		}
	}
	if r.queryLease != nil && !r.queryLease.validLocked() {
		return ErrAccountRequestSuperseded
	}
	finished := time.Now().UTC()
	if finished.Before(r.startedAt) {
		finished = r.startedAt
	}
	batch := make([]AccountRequestObservation, 0, len(observations))
	for _, observation := range observations {
		if observation.Source != "" && observation.Source != r.source {
			continue
		}
		observation.Source, observation.StartedAt, observation.FinishedAt = r.source, r.startedAt, finished
		batch = append(batch, observation)
	}
	batch = sanitizeAccountRequestObservations(batch)
	if len(batch) == 0 {
		return nil
	}
	r.completed = true
	next := make([]AccountRequestObservation, 0, len(j.AccountRequestObservations)+len(batch))
	for _, old := range j.AccountRequestObservations {
		if old.Source != r.source {
			next = append(next, old)
		}
	}
	j.AccountRequestObservations = sanitizeAccountRequestObservations(append(next, batch...))
	// UpdatedAt participates in query authorization identity. Diagnostics must
	// never change it, task status, logs, credentials, or an active lease.
	m.persistLocked()
	return m.lastPersistError
}

func accountRequestError(code string) (string, string) {
	switch code {
	case "authorization_rejected", "unauthorized":
		return code, "上游拒绝了账号授权"
	case "permission_denied", "forbidden", "unsupported_authorization":
		return code, "账号权限受限"
	case "rate_limited":
		return code, "账号请求受到上游限流"
	case "timeout":
		return code, "账号请求超时"
	case "network_error", "transport_error":
		return code, "账号网络请求失败"
	case "identity_mismatch", "invalid_credentials", "identity_unverified":
		return code, "账号授权身份校验失败"
	case "authorization_unavailable", "authorization_expired", "recovery_pending":
		return code, "已保存的账号授权暂不可用"
	case "ownership_unavailable":
		return code, "本机账号互斥保护不可用，未发送请求"
	case "credentials_unavailable":
		return code, "安全存储暂不可读取，无法确认账号状态，未发送请求"
	case "proxy_selection_required", "proxy_unavailable", "transport_unavailable", "unsafe_transport":
		return code, "账号请求通道暂不可用"
	case "redirect_blocked":
		return code, "账号请求重定向已停止"
	case "response_too_large":
		return code, "账号响应超过大小限制"
	case "invalid_response", "invalid_json", "empty_response", "incomplete", "model_mismatch", "protocol_error", "challenge_or_html", "unexpected_content_type", "invalid_stream", "invalid_answer_format":
		return code, "账号响应未通过完整性校验"
	case "subscription_unconfirmed", "subscription_scope_mismatch":
		return code, "尚未确认账号套餐"
	case "model_unavailable", "refused", "probe_failed":
		return code, "账号模型请求未成功完成"
	case "invalid_options", "unsupported", "invalid_timezone", "endpoint_rejected":
		return code, "账号请求选项不可用"
	case "upstream_error", "http_error":
		return code, "账号上游请求失败"
	default:
		return "request_failed", "账号请求未成功完成"
	}
}

func sanitizeAccountRequestObservations(observations []AccountRequestObservation) []AccountRequestObservation {
	const perSourceLimit = 8
	type orderedObservation struct {
		value AccountRequestObservation
		index int
	}
	batches := make(map[string][]orderedObservation)
	for index, observation := range observations {
		if !accountRequestSource(observation.Source) || (observation.Status != "failed" && observation.Status != "success") || observation.FinishedAt.IsZero() || (observation.HTTPStatus != 0 && (observation.HTTPStatus < 100 || observation.HTTPStatus > 599)) {
			continue
		}
		if observation.StartedAt.IsZero() {
			observation.StartedAt = observation.FinishedAt
		}
		// Derived from the current account operation at the public boundary.
		observation.Historical = false
		if observation.StartedAt.After(observation.FinishedAt) {
			continue
		}
		if observation.Status == "success" {
			if observation.HTTPStatus != 0 && (observation.HTTPStatus < 200 || observation.HTTPStatus >= 300) {
				continue
			}
			observation.ErrorCode, observation.Message = "", ""
		} else {
			observation.ErrorCode, observation.Message = accountRequestError(observation.ErrorCode)
		}
		batch := batches[observation.Source]
		if len(batch) > 0 && observation.StartedAt.Before(batch[0].value.StartedAt) {
			continue
		}
		if len(batch) > 0 && observation.StartedAt.After(batch[0].value.StartedAt) {
			batch = nil
		}
		if len(batch) > 0 {
			if observation.Status == "success" && batch[0].value.Status == "failed" {
				continue
			}
			if observation.Status == "failed" && batch[0].value.Status == "success" {
				batch = nil
			}
		}
		// Within a result batch timestamps are shared. Preserve observation
		// order, and let the last occurrence of a repeated status win.
		for i, old := range batch {
			if old.value.Status == observation.Status && old.value.HTTPStatus == observation.HTTPStatus && old.value.ErrorCode == observation.ErrorCode {
				batch = append(batch[:i], batch[i+1:]...)
				break
			}
		}
		batch = append(batch, orderedObservation{value: observation, index: index})
		if len(batch) > perSourceLimit {
			batch = batch[len(batch)-perSourceLimit:]
		}
		batches[observation.Source] = batch
	}
	var ordered []orderedObservation
	for _, batch := range batches {
		ordered = append(ordered, batch...)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	var out []AccountRequestObservation
	for _, observation := range ordered {
		out = append(out, observation.value)
	}
	return out
}
