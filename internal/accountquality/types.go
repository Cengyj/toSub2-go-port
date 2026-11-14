// Package accountquality schedules explicit model-quality checks independently
// of authentication and retains only bounded, credential-free observations.
package accountquality

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

var (
	ErrDisabled             = errors.New("模型质量检测未启用")
	ErrClosed               = errors.New("模型质量检测服务未启动或已关闭")
	ErrConflict             = errors.New("检测设置已被其他页面修改，请重新读取")
	ErrSettingsUnavailable  = errors.New("检测设置无法可靠读取，已停止检测与上传")
	ErrPersistence          = errors.New("检测设置未能可靠保存，请重新读取并确认")
	ErrPersistenceUncertain = errors.New("检测设置持久性未确认，已停止检测与上传")
	ErrUploadBlocked        = errors.New("上传前模型质量检测未通过，未开始上传")
	ErrBusy                 = errors.New("账号已有不同身份的检测进行中")
	ErrSuperseded           = errors.New("账号授权或检测设置已改变，本次结果未采用")
	ErrCacheBusy            = errors.New("账号仍在活动列表或检测尚未结束，结果暂不可清理")
	ErrCachePersistence     = errors.New("检测结果未能可靠保存")
	ErrCacheCleanup         = errors.New("模型检测结果未完全清理，请检查存储后重试")
)

type Settings struct {
	SchemaVersion      int    `json:"schemaVersion"`
	Revision           uint64 `json:"revision"`
	Enabled            bool   `json:"enabled"`
	BeforeUpload       bool   `json:"beforeUpload"`
	BlockDegraded      bool   `json:"blockDegraded"`
	BlockFailed        bool   `json:"blockFailed"`
	Model              string `json:"model"`
	ReasoningEffort    string `json:"reasoningEffort"`
	MinimumScore       int    `json:"minimumScore"`
	TimeoutSeconds     int    `json:"timeoutSeconds"`
	CooldownSeconds    int    `json:"cooldownSeconds"`
	ResultTTLSeconds   int    `json:"resultTTLSeconds"`
	PersistenceWarning string `json:"persistenceWarning,omitempty"`
}

// DecodeSettingsJSON applies the same complete, explicit policy contract to
// API writes and durable reads. A partial request must never disable a switch.
func DecodeSettingsJSON(data []byte) (Settings, error) {
	var document policyDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return Settings{}, err
	}
	if err := validateSettings(document.Settings); err != nil {
		return Settings{}, err
	}
	return document.Settings, nil
}

func DefaultSettings() Settings {
	return Settings{SchemaVersion: 1, Revision: 1, BlockDegraded: true, BlockFailed: true,
		ReasoningEffort: "high", MinimumScore: modelprobe.DefaultMinimumScore, TimeoutSeconds: 90, CooldownSeconds: 60, ResultTTLSeconds: 900}
}

// Key is a private stable same-account key shared by historical job IDs. It
// must not contain credentials and is never emitted or persisted by the service.
type Account struct {
	ID, Key, Revision string
	CanCheck          bool
	// Reason is a public-safe eligibility message supplied by the application.
	Reason string
}

type Lease struct {
	Context  context.Context
	Identity string
	Validate func() error
	Release  func()
	Probe    func(context.Context, modelprobe.Options) (modelprobe.Result, error)
}

type Provider struct {
	Accounts    func() []Account
	RetainedIDs func() []string
	Acquire     func(context.Context, string) (*Lease, error)
}

// UploadProbe borrows the caller's existing account ownership. CheckForUpload
// never calls Provider.Acquire. Identity must describe the exact committed grant
// and transport identity; only its digest is retained in private cache metadata.
type UploadProbe struct {
	Identity string
	Validate func() error
	Run      func(context.Context, modelprobe.Options) (modelprobe.Result, error)
}

type Attempt struct {
	CheckID    string     `json:"checkId"`
	Source     string     `json:"source"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	ErrorCode  string     `json:"errorCode,omitempty"`
	HTTPStatus int        `json:"httpStatus,omitempty"`
	Message    string     `json:"message,omitempty"`
}

// HTTPObservation retains the most recent explicit transport observation. It
// does not turn a failed, cancelled, or incomplete probe into a scored result.
type HTTPObservation struct {
	Status     string    `json:"status"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
	ErrorCode  string    `json:"errorCode,omitempty"`
	Message    string    `json:"message,omitempty"`
	FinishedAt time.Time `json:"finishedAt"`
	Source     string    `json:"source"`
	Model      string    `json:"model,omitempty"`
}

// LastResult is the last valid observation, including a degraded verdict.
// A failed or cancelled latest attempt never silently becomes a passing result.
type Snapshot struct {
	ID                string             `json:"id"`
	LastAttempt       Attempt            `json:"lastAttempt"`
	LastResult        *modelprobe.Result `json:"lastResult,omitempty"`
	HTTPObservation   *HTTPObservation   `json:"httpObservation,omitempty"`
	ProbeHistory      ProbeHistory       `json:"probeHistory,omitempty"`
	NextAllowedAt     *time.Time         `json:"nextAllowedAt,omitempty"`
	State             string             `json:"state"`
	CanCheck          bool               `json:"canCheck"`
	UnavailableReason string             `json:"unavailableReason,omitempty"`
	Stale             bool               `json:"stale"`
	StaleReason       string             `json:"staleReason,omitempty"`
	Persisted         bool               `json:"persisted"`
	Revision          uint64             `json:"revision"`
}

type Skip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type Receipt struct {
	CheckID  string   `json:"checkId"`
	Accepted []string `json:"accepted"`
	Merged   []string `json:"merged"`
	Skipped  []Skip   `json:"skipped"`
}

type CancelReceipt struct {
	Cancelled []string `json:"cancelled"`
	Unchanged []string `json:"unchanged"`
}

type Status struct {
	Enabled           bool   `json:"enabled"`
	BeforeUpload      bool   `json:"beforeUpload"`
	Running           bool   `json:"running"`
	Queued            int    `json:"queued"`
	Active            int    `json:"active"`
	SettingsAvailable bool   `json:"settingsAvailable"`
	SettingsRevision  uint64 `json:"settingsRevision"`
	LastError         string `json:"lastError,omitempty"`
	Revision          uint64 `json:"revision"`
}

type UploadDecision struct {
	Allowed   bool               `json:"allowed"`
	Checked   bool               `json:"checked"`
	Cached    bool               `json:"cached"`
	Result    *modelprobe.Result `json:"result,omitempty"`
	ErrorCode string             `json:"errorCode,omitempty"`
	Message   string             `json:"message,omitempty"`
}

// GateError always means the Sub2 mutation has not started. Its cause can be
// inspected with errors.Is without including upstream response text in logs.
type GateError struct {
	Code  string
	Cause error
}

func (e *GateError) Error() string                { return gateMessage(e.Code) }
func (e *GateError) Unwrap() error                { return e.Cause }
func (e *GateError) UploadOutcomeUncertain() bool { return false }

func gateMessage(code string) string {
	switch code {
	case "settings_unavailable":
		return ErrSettingsUnavailable.Error()
	case "persistence_failed":
		return "检测结果未能可靠保存，未开始上传"
	case "closed":
		return "检测服务已关闭，未开始上传"
	case "cancelled":
		return "模型质量检测已取消，未开始上传"
	case "superseded":
		return "账号授权或检测设置已改变，未开始上传"
	case "degraded":
		return "糖果题未答对，未开始上传"
	case "cooldown":
		return "模型质量检测仍在冷却，请稍后重试上传"
	case "busy":
		return "账号检测尚未结束，未开始上传"
	default:
		if message := probeMessage(code); message != "" {
			return message + "，未开始上传"
		}
		return ErrUploadBlocked.Error()
	}
}
