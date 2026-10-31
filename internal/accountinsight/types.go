// Package accountinsight collects read-only observations independently of login jobs.
package accountinsight

import (
	"context"
	"errors"
	"time"
	"tosub2-go-port/internal/protocol"
)

var ErrDisabled = errors.New("账号查询功能未启用")
var ErrConflict = errors.New("查询设置已被其他页面修改，请重新读取")
var ErrClosed = errors.New("账号查询服务尚未启动或已关闭")
var ErrPersistence = errors.New("查询设置未保存，请检查存储后重试")
var ErrPersistenceUncertain = errors.New("查询设置持久性未确认，本次已停止；请重新读取并明确保存")

type Settings struct {
	SchemaVersion       int      `json:"schemaVersion"`
	Revision            uint64   `json:"revision"`
	Enabled             bool     `json:"enabled"`
	Automatic           bool     `json:"automatic"`
	IntervalMinutes     int      `json:"intervalMinutes"`
	IncludeResetCredits bool     `json:"includeResetCredits"`
	ExcludedAccountIDs  []string `json:"excludedAccountIds"`
	PersistenceWarning  string   `json:"persistenceWarning,omitempty"`
}

func DefaultSettings() Settings {
	return Settings{SchemaVersion: 1, Automatic: true, IntervalMinutes: 15, ExcludedAccountIDs: []string{}}
}

type Account struct {
	ID, AuthorizationMethod, Revision string
	CanQuery                          bool
	Reason                            string
}

// Lease retains the manager-owned reservation until the safe result commit.
// Identity is private and never included in public JSON or logs.
type Lease struct {
	Context  context.Context
	Identity string
	Validate func() error
	Release  func()
	Query    func(context.Context, protocol.InsightsOptions) protocol.InsightResult
}
type Provider struct {
	Accounts func() []Account
	Acquire  func(context.Context, string) (*Lease, error)
	Timezone func() string
}
type Attempt struct {
	QueryID    string     `json:"queryId"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	ErrorCode  string     `json:"errorCode,omitempty"`
	Message    string     `json:"message,omitempty"`
}
type Snapshot struct {
	protocol.InsightResult
	ID                  string                    `json:"id"`
	AuthorizationMethod string                    `json:"authorizationMethod"`
	LastAttempt         Attempt                   `json:"lastAttempt"`
	LastSuccessAt       *time.Time                `json:"lastSuccessAt,omitempty"`
	NextCheckAt         *time.Time                `json:"nextCheckAt,omitempty"`
	NextAllowedAt       *time.Time                `json:"nextAllowedAt,omitempty"`
	CanQuery            bool                      `json:"canQuery"`
	UnavailableReason   string                    `json:"unavailableReason,omitempty"`
	Automatic           bool                      `json:"automatic"`
	Persisted           bool                      `json:"persisted"`
	Stale               bool                      `json:"stale"`
	Revision            uint64                    `json:"revision"`
	Components          map[string]ComponentState `json:"components,omitempty"`
}
type ComponentState struct {
	NextAllowedAt       *time.Time             `json:"nextAllowedAt,omitempty"`
	ConsecutiveFailures int                    `json:"consecutiveFailures"`
	LastError           *protocol.InsightError `json:"lastError,omitempty"`
}
type storedSnapshot struct {
	SchemaVersion   int      `json:"schemaVersion"`
	AccountRevision string   `json:"accountRevision"`
	Snapshot        Snapshot `json:"snapshot"`
	BlockedCode     string   `json:"blockedCode,omitempty"`
	BlockedReason   string   `json:"blockedReason,omitempty"`
}
type Skip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type Receipt struct {
	QueryID  string   `json:"queryId"`
	Accepted []string `json:"accepted"`
	Merged   []string `json:"merged"`
	Skipped  []Skip   `json:"skipped"`
}
type Counts struct {
	Success   int `json:"success"`
	Partial   int `json:"partial"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Cancelled int `json:"cancelled"`
}
type Status struct {
	Enabled     bool       `json:"enabled"`
	Automatic   bool       `json:"automatic"`
	Running     bool       `json:"running"`
	Queued      int        `json:"queued"`
	Active      int        `json:"active"`
	NextCheckAt *time.Time `json:"nextCheckAt,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	Counts      Counts     `json:"counts"`
	Revision    uint64     `json:"revision"`
}
