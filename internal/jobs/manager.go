package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"log/slog"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

const (
	StatusImported     = "imported"
	StatusQueued       = "queued"
	StatusRunning      = "running"
	StatusWaitingInput = "waiting_input"
	StatusSucceeded    = "completed"
	StatusCompleted    = StatusSucceeded
	StatusFailed       = "failed"
	StatusCanceled     = "canceled"
)

type Job struct {
	totpRecoveryCleanup bool
	passwordCleanup     bool
	totpSetupCleanup    bool
	ID                  string    `json:"id"`
	Email               string    `json:"email,omitempty"`
	Operation           string    `json:"operation,omitempty"`
	LastOperationType   string    `json:"lastOperationType,omitempty"`
	LastOperationAt     time.Time `json:"lastOperationAt,omitempty"`
	OperationStatus     string    `json:"operationStatus,omitempty"`
	OperationError      string    `json:"operationError,omitempty"`
	Status              string    `json:"status"`
	Stage               string    `json:"stage,omitempty"`
	Error               string    `json:"error,omitempty"`
	// Prompt is a user-facing description of the current input requested by
	// the protocol. It is intentionally metadata only; sensitive values live
	// in Input and are never serialized.
	Prompt                     string     `json:"prompt,omitempty"`
	SecurityActionReturnStatus string     `json:"securityActionReturnStatus,omitempty"`
	QueuedAt                   time.Time  `json:"queuedAt"`
	CreatedAt                  time.Time  `json:"createdAt"`
	UpdatedAt                  time.Time  `json:"updatedAt"`
	StartedAt                  *time.Time `json:"startedAt,omitempty"`
	FinishedAt                 *time.Time `json:"finishedAt,omitempty"`
	Attempts                   int        `json:"attempts"`
	Logs                       []string   `json:"logs,omitempty"`
	LogEntries                 []LogEntry `json:"logEntries,omitempty"`
	LastError                  string     `json:"lastError,omitempty"`
	ResultSaved                bool       `json:"resultSaved,omitempty"`
	PlanType                   string     `json:"planType,omitempty"`
	LoginMode                  string     `json:"loginMode,omitempty"`
	AuthorizationMethod        string     `json:"authorizationMethod,omitempty"`
	MailAPIURL                 string     `json:"-"`
	MailStatus                 string     `json:"mailStatus,omitempty"`
	MailAPIError               string     `json:"mailApiError,omitempty"`
	CurrentPhone               string     `json:"currentPhone,omitempty"`
	PhoneError                 string     `json:"phoneError,omitempty"`
	PhoneRetryRequired         bool       `json:"phoneRetryRequired,omitempty"`
	// TOTP setup material is process-local. It must never be written to the
	// ordinary job metadata file; an authorized one-time operation may expose
	// it through a dedicated flow if the UI needs to display it.
	TOTPSetupSecret             string          `json:"-"`
	TOTPSetupURI                string          `json:"-"`
	TOTPSetupError              string          `json:"totpSetupError,omitempty"`
	ResetTOTP                   bool            `json:"resetTotp,omitempty"`
	TOTPResetStatus             string          `json:"totpResetStatus,omitempty"`
	TOTPResetPhase              string          `json:"totpResetPhase,omitempty"`
	TOTPResetError              string          `json:"totpResetError,omitempty"`
	TOTPRecoveryPending         bool            `json:"totpRecoveryPending,omitempty"`
	TOTPCredentialInvalidated   bool            `json:"totpCredentialInvalidated,omitempty"`
	LogoutAllDevicesAfterTOTP   bool            `json:"logoutAllDevicesAfterTotp,omitempty"`
	LogoutAllDevicesStatus      string          `json:"logoutAllDevicesStatus,omitempty"`
	LogoutAllDevicesError       string          `json:"logoutAllDevicesError,omitempty"`
	ReloginAfterTOTP            bool            `json:"reloginAfterTotp,omitempty"`
	UploadAfterTOTP             bool            `json:"uploadAfterTotp,omitempty"`
	TOTPWorkflowStatus          string          `json:"totpWorkflowStatus,omitempty"`
	TOTPWorkflowStep            string          `json:"totpWorkflowStep,omitempty"`
	TOTPWorkflowError           string          `json:"totpWorkflowError,omitempty"`
	TOTPReloginStatus           string          `json:"totpReloginStatus,omitempty"`
	TOTPUploadStatus            string          `json:"totpUploadStatus,omitempty"`
	UploadAfterRelogin          bool            `json:"uploadAfterRelogin,omitempty"`
	ReloginAuthorizationSaved   bool            `json:"reloginAuthorizationSaved,omitempty"`
	ReloginUploadStatus         string          `json:"reloginUploadStatus,omitempty"`
	ReloginUploadError          string          `json:"reloginUploadError,omitempty"`
	PasswordAddError            string          `json:"passwordAddError,omitempty"`
	PasswordAddedAt             *time.Time      `json:"passwordAddedAt,omitempty"`
	HasTOTPKey                  bool            `json:"hasTotpKey,omitempty"`
	HasPassword                 bool            `json:"hasPassword,omitempty"`
	TOTPKnownEnabled            bool            `json:"totpKnownEnabled,omitempty"`
	SMSProviderID               string          `json:"smsProviderId,omitempty"`
	SMSProviderName             string          `json:"smsProviderName,omitempty"`
	SMSServiceLabel             string          `json:"smsServiceLabel,omitempty"`
	SMSStatus                   string          `json:"smsStatus,omitempty"`
	SMSError                    string          `json:"smsError,omitempty"`
	SecurityCheckRequired       bool            `json:"securityCheckRequired,omitempty"`
	ProxyConfigured             bool            `json:"proxyConfigured,omitempty"`
	AutoRepairBlocked           bool            `json:"autoRepairBlocked,omitempty"`
	AutoRepairBlockedReason     string          `json:"autoRepairBlockedReason,omitempty"`
	AutoRepairPendingAccountIDs []int           `json:"autoRepairPendingAccountIds,omitempty"`
	AutoRepairBackend           string          `json:"autoRepairBackend,omitempty"`
	AutoRepairLastAttemptAt     *time.Time      `json:"autoRepairLastAttemptAt,omitempty"`
	AutoRepairLastSuccessAt     *time.Time      `json:"autoRepairLastSuccessAt,omitempty"`
	AutoRepairLastError         string          `json:"autoRepairLastError,omitempty"`
	LastAuthAutomated           bool            `json:"lastAuthAutomated"`
	LastAuthAutomationReason    string          `json:"lastAuthAutomationReason,omitempty"`
	LastAuthAutomatedAt         *time.Time      `json:"lastAuthAutomatedAt,omitempty"`
	LastAuthRequirements        map[string]bool `json:"lastAuthRequirements,omitempty"`
	Input                       map[string]any  `json:"-"`
}

type JobEvent struct {
	JobID  string         `json:"jobId"`
	Stage  string         `json:"stage,omitempty"`
	Status string         `json:"status"`
	Prompt string         `json:"prompt,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
	At     time.Time      `json:"at"`
}

type Spec struct {
	ID         string
	Email      string
	Operation  string
	Input      map[string]any
	ImportOnly bool
}

// Runner performs protocol work for a job. Returning context cancellation is
// recorded as canceled; all other errors are retained as a safe message.
type Runner func(context.Context, *Job, func(JobEvent)) error

// Manager is a FIFO scheduler with a global concurrency limit and one active
// job per email address. Jobs are persisted as sanitized metadata.
type Manager struct {
	mu               sync.Mutex
	jobs             map[string]*Job
	pending          []*Job
	active           map[string]context.CancelFunc
	inputPreparing   map[string]*inputPreparation
	emailUse         map[string]string
	restart          map[string]bool
	deleting         map[string]bool
	importing        map[string]bool
	importGate       chan struct{}
	lastOutputSyncAt time.Time
	max              int
	runner           Runner
	rootCtx          context.Context
	cancel           context.CancelFunc
	wake             chan struct{}
	events           map[chan JobEvent]struct{}
	store            *store.Store
	logger           *slog.Logger
	vault            credentials.Store
	startOnce        sync.Once
	closing          bool
	lastPersistError error
	wg               sync.WaitGroup
}

func NewManager(parent context.Context, max int, st *store.Store, logger *slog.Logger) *Manager {
	if max < 1 {
		max = 1
	}
	if max > 20 {
		max = 20
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{jobs: make(map[string]*Job), active: make(map[string]context.CancelFunc), emailUse: make(map[string]string), restart: make(map[string]bool), deleting: make(map[string]bool), importing: make(map[string]bool), importGate: make(chan struct{}, 1), max: max, rootCtx: ctx, cancel: cancel, wake: make(chan struct{}, 1), events: make(map[chan JobEvent]struct{}), store: st, logger: logger}
}
func (m *Manager) SetRunner(r Runner)                     { m.mu.Lock(); m.runner = r; m.mu.Unlock() }
func (m *Manager) SetCredentialStore(v credentials.Store) { m.mu.Lock(); m.vault = v; m.mu.Unlock() }
func (m *Manager) Start()                                 { m.startOnce.Do(func() { m.wg.Add(1); go m.schedule() }) }
func (m *Manager) schedule() {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		for !m.closing && m.rootCtx.Err() == nil && len(m.active) < m.max {
			index := m.nextRunnableLocked()
			if index < 0 {
				break
			}
			job := m.pending[index]
			m.pending = append(m.pending[:index], m.pending[index+1:]...)
			ctx, cancel := context.WithCancel(m.rootCtx)
			m.active[job.ID] = cancel
			if job.Email != "" {
				m.emailUse[emailKey(job.Email)] = job.ID
			}
			now := time.Now().UTC()
			job.Status = StatusRunning
			job.StartedAt = &now
			job.UpdatedAt = now
			if job.SecurityActionReturnStatus != StatusImported || !isSuboperation(job.Operation) {
				job.Attempts++
			}
			m.emitLocked(JobEvent{JobID: job.ID, Status: job.Status, Stage: "start", At: now})
			m.wg.Add(1)
			go m.run(ctx, cancel, cloneJob(job))
		}
		m.mu.Unlock()
		select {
		case <-m.rootCtx.Done():
			return
		case <-m.wake:
		}
	}
}
func (m *Manager) nextRunnableLocked() int {
	for i, job := range m.pending {
		if m.deleting[emailKey(job.Email)] || m.importing[emailKey(job.Email)] {
			continue
		}
		if job.Email == "" || m.emailUse[emailKey(job.Email)] == "" {
			return i
		}
	}
	return -1
}
func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, job *Job) {
	defer m.wg.Done()
	// Each attempt owns its context even when it finishes normally. Release
	// descendants and AfterFunc callbacks after final status classification,
	// so cleanup cannot turn a successful or failed action into a cancellation.
	defer cancel()
	m.mu.Lock()
	runner := m.runner
	m.mu.Unlock()
	if runner == nil {
		runner = func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
			emit(JobEvent{Stage: "ready", Status: StatusRunning})
			return nil
		}
	}
	err := runner(ctx, job, func(e JobEvent) { e.JobID = job.ID; m.Emit(e) })
	m.mu.Lock()
	defer m.mu.Unlock()
	job = m.jobs[job.ID]
	delete(m.active, job.ID)
	if job.Email != "" {
		delete(m.emailUse, emailKey(job.Email))
	}
	now := time.Now().UTC()
	job.UpdatedAt = now
	job.FinishedAt = &now
	restart := m.restart[job.ID]
	delete(m.restart, job.ID)
	if job.TOTPRecoveryPending || job.TOTPCredentialInvalidated {
		job.Status, job.Error = StatusFailed, ErrTOTPRecoveryPending.Error()
		job.Prompt = "2FA 重置未完成，需要手动核实；不会自动重试移除或退出"
		if job.TOTPResetStatus == "disabling" || job.TOTPResetStatus == "disabled" {
			job.TOTPResetStatus = "unknown"
		}
	} else if (job.TOTPWorkflowStatus == "failed" || job.TOTPWorkflowStatus == "interrupted") && (job.ReloginAfterTOTP || job.UploadAfterTOTP) && totpOperation(job.Operation) && err != nil {
		job.Status, job.Error = StatusFailed, safeError(err)
	} else if job.ResetTOTP && err != nil {
		job.Status, job.Error = StatusFailed, safeError(err)
	} else if m.closing || m.rootCtx.Err() != nil {
		job.Status = "interrupted"
		job.Error = "服务关闭，任务已中断，可恢复"
	} else if restart {
		job.Status = StatusQueued
		job.Operation = "full"
		initializeTOTPWorkflow(job, nil)
		job.LastOperationType, job.LastOperationAt = "reauthorize", now
		job.Stage = "queued"
		job.Prompt = "账号资料已更新，正在重新建立登录会话"
		job.QueuedAt = now
		job.FinishedAt = nil
		job.Error = ""
		m.pending = append(m.pending, job)
	} else if ctx.Err() != nil {
		// Only cancellation of this job's context is a task cancellation.
		// Browser/provider subcontexts have their own deadlines and cleanup;
		// preserve those operation failures instead of replacing them with
		// "canceled" and losing the actionable challenge/transport diagnostic.
		if !restoreCanceledSuboperation(job) {
			job.Status = StatusCanceled
			job.Error = "canceled"
			job.Prompt = "操作已取消"
		}
	} else if totpOperation(job.Operation) && err == nil && job.TOTPWorkflowStatus == "succeeded" && job.ReloginAfterTOTP {
		job.Status, job.Stage, job.Prompt = StatusSucceeded, "complete", "2FA 与重新授权流程已完成"
		if job.UploadAfterTOTP {
			job.Prompt = "2FA、重新授权和 Sub2API 同步已完成"
		}
	} else if job.ResetTOTP && job.TOTPResetPhase == "credential_persisted" {
		job.Status, job.Stage, job.Prompt = StatusSucceeded, "complete", "2FA 已重置，新密钥已安全保存"
		if !job.ResultSaved {
			job.Status = "reauth_required"
			job.Prompt = "2FA 已重置，新密钥已安全保存；原登录会话已失效，请重新登录获取授权"
		}
		if job.SecurityActionReturnStatus == StatusImported {
			job.Status, job.Stage, job.QueuedAt = StatusImported, StatusImported, time.Time{}
			job.Prompt = "2FA 已重置，新密钥已安全保存，请选择要执行的功能"
		}
	} else if isSuboperation(job.Operation) {
		job.Status = StatusSucceeded
		if !job.ResultSaved {
			job.Status = "resume_available"
		}
		if job.SecurityActionReturnStatus == StatusImported {
			job.Status, job.Stage, job.Prompt = StatusImported, StatusImported, "操作已结束，请选择要执行的功能"
			job.QueuedAt = time.Time{}
		}
		if err != nil {
			job.Error = safeError(err)
		}
	} else if err != nil {
		job.Status = StatusFailed
		job.Error = safeError(err)
	} else {
		job.Status = StatusSucceeded
		job.Stage = "complete"
	}
	// Keep the action outcome separate from retained authorization/checkpoints.
	job.OperationStatus, job.OperationError = "succeeded", ""
	switch {
	case restart:
		beginOperationState(job)
		job.Prompt = "账号资料已更新，正在重新建立登录会话"
	case job.TOTPRecoveryPending || job.TOTPCredentialInvalidated:
		job.OperationStatus, job.OperationError = "failed", job.Error
	case m.closing || m.rootCtx.Err() != nil:
		job.OperationStatus, job.OperationError = "interrupted", job.Error
	case ctx.Err() != nil:
		job.OperationStatus = "canceled"
	case err != nil:
		job.OperationStatus, job.OperationError = "failed", safeError(err)
		if totpOperation(job.Operation) && job.TOTPWorkflowError != "" {
			job.OperationError = job.TOTPWorkflowError
			job.Error = job.TOTPWorkflowError
		}
	}
	if job.OperationStatus == "succeeded" {
		job.Error = ""
		job.Prompt = operationCompletionPrompt(job)
	} else if job.OperationStatus == "failed" || job.OperationStatus == "interrupted" {
		job.Prompt = job.OperationError
	}
	if pendingRefresh := m.pendingRefreshOperation(job); pendingRefresh != "" && err != nil {
		job.Operation, job.Stage = pendingRefresh, refreshRecoveryStage
		job.Prompt = "刷新授权已保存在私有恢复文件中，可以重试完成本地保存"
	}
	m.emitLocked(JobEvent{JobID: job.ID, Status: job.Status, Stage: job.Stage, Prompt: job.Prompt, At: now, Data: map[string]any{"error": firstOperationErrorForLog(job)}})
	if job.LastError != "" {
		m.appendOperationLogLocked(job, JobEvent{JobID: job.ID, Stage: "credential_warning", At: now, Data: map[string]any{"mailApiError": job.LastError}})
	}
	m.persistLocked()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) Submit(spec Spec) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.submitLocked(spec)
}
func (m *Manager) submitLocked(spec Spec) (*Job, error) {
	method, err := AuthorizationMethodForValues("", spec.Input)
	if err != nil {
		return nil, err
	}
	if err := m.checkTOTPActionLocked(spec.Email, &Job{ID: spec.ID, Operation: spec.Operation}); err != nil {
		return nil, err
	}
	if m.deleting[emailKey(spec.Email)] || m.importing[emailKey(spec.Email)] {
		return nil, errors.New("account is being deleted or restored")
	}
	if m.closing || m.rootCtx.Err() != nil {
		return nil, errors.New("job manager is shutting down")
	}
	id := spec.ID
	if id != "" && !safeID(id) {
		return nil, fmt.Errorf("invalid job id")
	}
	if id == "" {
		id = newID()
	}
	if _, exists := m.jobs[id]; exists {
		return nil, fmt.Errorf("job %s already exists", id)
	}
	now := time.Now().UTC()
	job := &Job{ID: id, Email: spec.Email, Operation: spec.Operation, Status: StatusQueued, QueuedAt: now, CreatedAt: now, UpdatedAt: now, LastOperationAt: now, Input: cloneMap(spec.Input), LoginMode: "email_otp"}
	job.AuthorizationMethod = method
	beginOperationState(job)
	job.ResetTOTP = resetOperation(spec.Operation)
	initializeTOTPWorkflow(job, spec.Input)
	if spec.ImportOnly {
		job.OperationStatus = ""
		job.Status, job.Stage, job.Operation, job.LastOperationType = StatusImported, StatusImported, "", "account_import"
		job.Prompt, job.QueuedAt = "已导入，请选择要执行的功能", time.Time{}
	}
	if v, ok := spec.Input["password"].(string); ok && v != "" {
		job.HasPassword = true
		job.LoginMode = "password"
	}
	if v, ok := spec.Input["totpSecret"].(string); ok && v != "" {
		job.HasTOTPKey = true
	}
	if v, ok := spec.Input["mailApiUrl"].(string); ok {
		job.MailAPIURL = v
	}
	if v, ok := spec.Input["proxyUrl"].(string); ok && v != "" {
		job.ProxyConfigured = true
	}
	m.jobs[id] = job
	if err := m.commitAdmissionLocked(job, nil, JobEvent{JobID: id, Status: job.Status, Stage: job.Status, At: now}); err != nil {
		return nil, err
	}
	m.saveSecretsLocked(job, spec.Input)
	return cloneJob(job), nil
}
func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return nil, false
	}
	return cloneJob(job), true
}
func (m *Manager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, cloneJob(j))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// FindByEmail returns the newest non-deleted job for an email address. Email
// comparisons follow the legacy console semantics (trimmed, case-insensitive)
// and the result is a defensive copy.
func (m *Manager) FindByEmail(email string) (*Job, bool) {
	key := emailKey(email)
	m.mu.Lock()
	defer m.mu.Unlock()
	var found *Job
	for _, j := range m.jobs {
		if emailKey(j.Email) != key {
			continue
		}
		if found == nil || j.CreatedAt.After(found.CreatedAt) || (j.CreatedAt.Equal(found.CreatedAt) && j.ID > found.ID) {
			found = j
		}
	}
	if found == nil {
		return nil, false
	}
	return cloneJob(found), true
}

// Update merges non-persistent input and optional metadata into a job. This is
// used by the compatibility console when a duplicate email is submitted. The
// input map is process-local and is deliberately excluded from job-meta.json.
func (m *Manager) Update(id string, values map[string]any) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateLocked(id, values)
}
func (m *Manager) updateLocked(id string, values map[string]any) (*Job, error) {
	if m.closing || m.rootCtx.Err() != nil {
		return nil, errors.New("job manager is shutting down")
	}
	j, ok := m.jobs[id]
	if !ok {
		return nil, osErrNotFound(id)
	}
	method, err := AuthorizationMethodForValues(j.AuthorizationMethod, values)
	if err != nil {
		return nil, err
	}
	if method != j.AuthorizationMethod && j.AuthorizationMethod != "" && (m.active[id] != nil || containsPending(m.pending, id)) {
		return nil, errors.New("任务进行中，不能更改授权方式")
	}
	if email, ok := values["email"].(string); ok && strings.TrimSpace(email) != "" && emailKey(email) != emailKey(j.Email) {
		return nil, errors.New("a job email identity cannot be changed")
	}
	if err := m.checkTOTPActionLocked(j.Email); err != nil {
		return nil, err
	}
	if m.deleting[emailKey(j.Email)] || m.importing[emailKey(j.Email)] {
		return nil, errors.New("account is being deleted or restored")
	}
	before := cloneJob(j)
	if j.Input == nil {
		j.Input = map[string]any{}
	}
	j.AuthorizationMethod = method
	for k, v := range values {
		j.Input[k] = cloneValue(v)
	}
	if op, ok := values["operation"].(string); ok && strings.TrimSpace(op) != "" {
		j.Operation = strings.TrimSpace(op)
		j.ResetTOTP = resetOperation(j.Operation)
		initializeTOTPWorkflow(j, values)
		j.LastOperationType, j.LastOperationAt = j.Operation, time.Now().UTC()
	}
	if v, ok := values["mailApiUrl"].(string); ok {
		j.MailAPIURL = v
	}
	if v, ok := values["password"].(string); ok {
		j.HasPassword = v != ""
		j.LoginMode = "email_otp"
		if v != "" {
			j.LoginMode = "password"
		}
	}
	if v, ok := values["totpSecret"].(string); ok {
		j.HasTOTPKey = v != ""
	}
	if v, ok := values["proxyUrl"].(string); ok {
		j.ProxyConfigured = v != ""
	}
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	if m.lastPersistError != nil {
		*j = *before
		return nil, admissionPersistenceError(m.lastPersistError)
	}
	m.saveSecretsLocked(j, values)
	return cloneJob(j), nil
}

// LoadCredentials reads a job's credential material from the configured secure
// store for an explicit source export. It never exposes credentials in Job.
func (m *Manager) LoadCredentials(id string) (credentials.Credentials, error) {
	return m.LoadCredentialsContext(m.rootCtx, id)
}

func (m *Manager) LoadCredentialsContext(ctx context.Context, id string) (credentials.Credentials, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return credentials.Credentials{}, osErrNotFound(id)
	}
	vault, email := m.vault, j.Email
	m.mu.Unlock()
	if vault == nil {
		return credentials.Credentials{}, errors.New("secure credential store unavailable")
	}
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.rootCtx, cancel)
	defer stop()
	saved, err := credentials.LoadContext(readCtx, vault, email)
	m.mu.Lock()
	defer m.mu.Unlock()
	if saved.TOTPResetPending || m.totpBlockedLocked(email) {
		return credentials.Credentials{}, ErrTOTPRecoveryPending
	}
	for _, candidate := range m.jobs {
		if emailKey(candidate.Email) == emailKey(email) && resetOperation(candidate.Operation) && (containsPending(m.pending, candidate.ID) || m.active[candidate.ID] != nil) {
			return credentials.Credentials{}, ErrTOTPRecoveryRequired
		}
	}
	if err != nil {
		return credentials.Credentials{}, err
	}
	return saved, nil
}

// InputCopy is intentionally explicit: callers that need to execute a
// protocol operation may inspect process-local input, while HTTP responses
// should use the public job representation instead.
func (m *Manager) InputCopy(id string) (map[string]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(j.Input))
	for k, v := range j.Input {
		out[k] = cloneValue(v)
	}
	return out, true
}

// ConsumeInput atomically removes a submitted value so rejected OTPs cannot
// be reused in a tight retry loop. Secrets remain process-local.
func (m *Manager) ConsumeInput(id string, keys ...string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return "", false
	}
	passwordInput := false
	for _, key := range keys {
		if key == "password" {
			passwordInput = true
			break
		}
	}
	for _, key := range keys {
		v, ok := j.Input[key].(string)
		if !passwordInput {
			v = strings.TrimSpace(v)
		}
		if ok && v != "" {
			for _, alias := range keys {
				delete(j.Input, alias)
			}
			return v, true
		}
	}
	return "", false
}

// AutoRepairState stores monitor bookkeeping without any credentials.
func (m *Manager) UpdateAutoRepair(id string, accountIDs []int, backend, lastError string, blocked bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return false
	}
	j.AutoRepairPendingAccountIDs = append([]int(nil), accountIDs...)
	j.AutoRepairBackend = backend
	j.AutoRepairLastError = lastError
	j.AutoRepairBlocked = blocked
	if blocked {
		j.AutoRepairBlockedReason = safeError(errors.New(lastError))
		if j.AutoRepairBlockedReason == "" {
			j.AutoRepairBlockedReason = "账号已确认封禁、删除或永久停用"
		}
		j.AutoRepairPendingAccountIDs = nil
		j.AutoRepairBackend = ""
	}
	now := time.Now().UTC()
	j.AutoRepairLastAttemptAt = &now
	j.UpdatedAt = now
	m.persistLocked()
	return true
}
func (m *Manager) ClearAutoRepair(id string, success bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return false
	}
	j.AutoRepairPendingAccountIDs = nil
	j.AutoRepairBackend = ""
	j.AutoRepairLastError = ""
	if success {
		now := time.Now().UTC()
		j.AutoRepairLastSuccessAt = &now
		j.AutoRepairBlocked = false
		j.AutoRepairBlockedReason = ""
	}
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	return true
}

// UpdateSMS records non-secret phone provider state for the console. Provider
// credentials and leases remain process-local in the console server.
func (m *Manager) UpdateSMS(id, providerID, providerName, number, status, smsError string, serviceLabel ...string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return false
	}
	if j.SMSProviderID != providerID || status == "idle" {
		j.SMSServiceLabel = ""
	}
	if len(serviceLabel) > 0 && status != "idle" {
		j.SMSServiceLabel = SanitizeSMSServiceLabel(serviceLabel[0])
	}
	j.SMSProviderID = providerID
	j.SMSProviderName = providerName
	j.CurrentPhone = number
	j.SMSStatus = status
	j.SMSError = smsError
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	return true
}

// UpdateResult changes metadata under the manager lock after protocol work.
func (m *Manager) UpdateResult(id string, saved bool, prompt string) {
	planType := "unknown"
	if saved && m.store != nil {
		if job, ok := m.Get(id); ok {
			planType = outputPlanType(m.store.Root, id, job.Email)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.ResultSaved = saved
		if j.Operation == "relogin" && j.UploadAfterRelogin && j.ReloginUploadStatus == "pending" {
			j.ReloginAuthorizationSaved = saved
		}
		if saved && planType != "unknown" {
			j.PlanType = planType
		}
		j.Prompt = prompt
		j.UpdatedAt = time.Now().UTC()
		m.persistLocked()
	}
}

// UpdateTOTPSetup keeps the setup secret only in memory for the dedicated
// authenticated setup response. It is excluded from job-meta.json via the
// json:"-" fields on Job.
func (m *Manager) UpdateTOTPSetup(id, secret, uri, setupErr string, enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.TOTPSetupSecret, j.TOTPSetupURI, j.TOTPSetupError = secret, uri, setupErr
		j.TOTPKnownEnabled = enabled
		if enabled && secret != "" {
			j.HasTOTPKey = true
		}
		j.UpdatedAt = time.Now().UTC()
		m.persistLocked()
	}
}
func (m *Manager) UpdatePasswordAdded(id string, added bool, passwordErr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.HasPassword = added
		j.PasswordAddError = passwordErr
		if added {
			j.LoginMode = "password"
			now := time.Now().UTC()
			j.PasswordAddedAt = &now
		}
		j.UpdatedAt = time.Now().UTC()
		m.persistLocked()
	}
}
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.restart, id)
	delete(m.inputPreparing, id)
	if cancel, ok := m.active[id]; ok {
		cancel()
		return true
	}
	if job, ok := m.jobs[id]; ok && job.Status == StatusQueued {
		interruptTOTPWorkflow(job, "流程已取消，后续步骤未执行")
		job.Status = StatusCanceled
		if !restoreCanceledSuboperation(job) {
			job.Prompt = "操作已取消"
		}
		job.OperationStatus, job.OperationError = "canceled", ""
		now := time.Now().UTC()
		job.UpdatedAt = now
		job.FinishedAt = &now
		m.removePendingLocked(id)
		m.emitLocked(JobEvent{JobID: id, Status: job.Status, Stage: "cancel", Prompt: job.Prompt, At: now})
		m.persistLocked()
		return true
	}
	return false
}
func (m *Manager) CancelAll() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for id, cancel := range m.active {
		delete(m.restart, id)
		delete(m.inputPreparing, id)
		cancel()
		_ = id
		count++
	}
	now := time.Now().UTC()
	for _, job := range m.pending {
		interruptTOTPWorkflow(job, "流程已取消，后续步骤未执行")
		job.Status = StatusCanceled
		if !restoreCanceledSuboperation(job) {
			job.Prompt = "操作已取消"
		}
		job.OperationStatus, job.OperationError = "canceled", ""
		job.UpdatedAt = now
		job.FinishedAt = &now
		m.emitLocked(JobEvent{JobID: job.ID, Status: job.Status, Stage: "cancel", Prompt: job.Prompt, At: now})
		count++
	}
	m.pending = nil
	m.persistLocked()
	return count
}

// Delete removes jobs that are not currently running and returns the number removed.
func (m *Manager) Delete(ids []string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	removed := 0
	for id := range set {
		if _, running := m.active[id]; running {
			continue
		}
		if _, ok := m.jobs[id]; !ok {
			continue
		}
		delete(m.jobs, id)
		m.removePendingLocked(id)
		removed++
	}
	if removed > 0 {
		m.persistLocked()
	}
	return removed
}

func (m *Manager) Retry(id string) (*Job, error) {
	return m.RetryWithValues(id, nil)
}

// RetryWithValues validates eligibility and applies edited credentials under
// the same lock, so a rejected retry cannot mutate a completed account.
func (m *Manager) RetryWithValues(id string, values map[string]any) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.rootCtx.Err() != nil {
		return nil, errors.New("job manager is shutting down")
	}
	job, ok := m.jobs[id]
	if !ok {
		return nil, osErrNotFound(id)
	}
	method, err := AuthorizationMethodForValues(job.AuthorizationMethod, values)
	if err != nil {
		return nil, err
	}
	previousMethod, _ := NormalizeAuthorizationMethod(job.AuthorizationMethod)
	if method != previousMethod && (job.Status == "resume_available" || m.pendingRefreshOperation(job) != "") {
		return nil, errors.New("请先完成当前授权的本地恢复，或使用重登授权选择其他方式")
	}
	if err := m.checkTOTPActionLocked(job.Email, job); err != nil {
		return nil, err
	}
	if m.deleting[emailKey(job.Email)] || m.importing[emailKey(job.Email)] {
		return nil, errors.New("account is being deleted or restored")
	}
	if job.Status != StatusImported && job.Status != StatusFailed && job.Status != StatusCanceled && job.Status != "resume_available" && job.Status != "reauth_required" {
		return nil, fmt.Errorf("job %s is not retryable", id)
	}
	if job.Status == StatusImported {
		var err error
		values, err = m.prepareImportedActionLocked(job, values)
		if err != nil {
			return nil, err
		}
	}
	before := cloneJob(job)
	now := time.Now().UTC()
	resume := job.Status == "resume_available"
	if !resume && job.SecurityCheckRequired && m.store != nil {
		_, _, resume = recoveryArtifactsForEmail(m.store.Root, id, job.Email)
	}
	if method != previousMethod {
		// A full run can reuse an existing checkpoint. Provider changes must
		// explicitly begin a new login, including after a security interruption.
		resume = false
	}
	pendingRefresh := m.pendingRefreshOperation(job)
	if pendingRefresh != "" {
		job.Operation, job.LastOperationType = pendingRefresh, "reauthorize"
		resume = false
	} else if resume {
		job.Operation = "resume"
		job.LastOperationType = "resume"
	} else {
		job.Operation = "full"
		if method != previousMethod {
			job.Operation = "relogin"
		}
		job.LastOperationType = "reauthorize"
		if job.Attempts == 0 {
			job.LastOperationType = "initial_authorization"
		}
	}
	job.SecurityActionReturnStatus = ""
	job.AuthorizationMethod = method
	job.ResetTOTP = false
	initializeTOTPWorkflow(job, nil)
	if job.Input == nil {
		job.Input = map[string]any{}
	}
	for key, value := range values {
		job.Input[key] = cloneValue(value)
	}
	if proxy, ok := values["proxyUrl"].(string); ok {
		job.ProxyConfigured = proxy != ""
	}
	if password, ok := values["password"].(string); ok {
		job.HasPassword = password != ""
		job.LoginMode = "email_otp"
		if password != "" {
			job.LoginMode = "password"
		}
	}
	if totp, ok := values["totpSecret"].(string); ok {
		job.HasTOTPKey = totp != ""
	}
	if mailURL, ok := values["mailApiUrl"].(string); ok {
		job.MailAPIURL = mailURL
	}
	if !resume {
		job.CurrentPhone = ""
		job.PhoneRetryRequired = false
	}
	clearManualRepairState(job)
	beginOperationState(job)
	job.LastOperationAt = now
	job.LastError = ""
	job.Stage, job.Prompt = "queued", "正在重新建立登录会话"
	if pendingRefresh != "" {
		job.Prompt = "正在完成已保存的刷新授权"
	} else if resume {
		job.Prompt = "正在继续已有登录检查点"
	}
	job.Status = StatusQueued
	job.QueuedAt = now
	job.Error = ""
	job.FinishedAt = nil
	job.UpdatedAt = now
	if err := m.commitAdmissionLocked(job, before, JobEvent{JobID: job.ID, Status: StatusQueued, Stage: "queued", Prompt: job.Prompt, At: now}); err != nil {
		return nil, err
	}
	m.saveSecretsLocked(job, values)
	return cloneJob(job), nil
}

// Input records a user supplied OTP or phone code without persisting the
// value in ordinary job metadata. The runner receives it through the in-memory
// Input map while the process is alive.
func (m *Manager) Input(id string, values map[string]any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inputLocked(id, "", values)
}

// InputForStage atomically checks the prompt and accepts its response. Once
// accepted, the console must stop offering that prompt while validation runs.
func (m *Manager) InputForStage(id, stage string, values map[string]any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stage == "" {
		return false
	}
	return m.inputLocked(id, stage, values)
}

// InputAutomaticSMS checks the retry pause under the same lock that admits
// the code. Subscriber delivery may lag behind a deferred protocol event, so
// a console-side snapshot alone must never authorize an automatic submission.
func (m *Manager) InputAutomaticSMS(id, code string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.PhoneRetryRequired {
		return false
	}
	return m.inputLocked(id, "phone_otp", map[string]any{"phone_otp": code})
}

type inputPreparation struct {
	job      *Job
	attempts int
}

// InputForStageWithPreparation closes the current prompt before preparing
// dependent state, but publishes no value until preparation has finished.
// SMS polling/release uses this to prevent a fast runner from verifying a code
// before its lease has been paused. A cancellation or new prompt invalidates
// the reservation instead of seeding input into a later run.
func (m *Manager) InputForStageWithPreparation(id, stage string, values map[string]any, prepare func()) bool {
	if prepare == nil {
		return m.InputForStage(id, stage, values)
	}
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok || stage == "" || job.Status != stage || m.closing || m.rootCtx.Err() != nil || m.deleting[emailKey(job.Email)] || m.importing[emailKey(job.Email)] {
		m.mu.Unlock()
		return false
	}
	if m.inputPreparing == nil {
		m.inputPreparing = make(map[string]*inputPreparation)
	}
	ticket := &inputPreparation{job: job, attempts: job.Attempts}
	m.inputPreparing[id] = ticket
	job.Status, job.Stage = "working", "working"
	job.Prompt = submittedInputPrompt(stage, values, job.CurrentPhone)
	job.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	m.mu.Unlock()

	prepare()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inputPreparing[id] != ticket {
		return false
	}
	delete(m.inputPreparing, id)
	current, ok := m.jobs[id]
	if !ok || current != job || current.Attempts != ticket.attempts || current.Status != "working" || current.Stage != "working" {
		return false
	}
	if !m.inputLocked(id, "", values) {
		return false
	}
	if stage == "phone" || stage == "phone_otp" {
		current.PhoneError = ""
		m.persistLocked()
	}
	return true
}

func (m *Manager) inputLocked(id, stage string, values map[string]any) bool {
	if m.closing || m.rootCtx.Err() != nil {
		return false
	}
	job, ok := m.jobs[id]
	if !ok {
		return false
	}
	if job.Status == StatusSucceeded || (stage != "" && job.Status != stage) {
		return false
	}
	if m.deleting[emailKey(job.Email)] || m.importing[emailKey(job.Email)] {
		return false
	}
	if m.totpBlockedLocked(job.Email) {
		_, active := m.active[id]
		if !active || !resetOperation(job.Operation) || stage != "totp_setup_otp" || len(values) != 1 {
			return false
		}
		if _, code := values["totp_setup_otp"].(string); !code {
			return false
		}
	}
	delete(m.inputPreparing, id)
	if job.Input == nil {
		job.Input = map[string]any{}
	}
	for k, v := range values {
		// Input is process-local and excluded from JSON persistence. Keep all
		// values here so password/TOTP and OAuth prompts can resume correctly.
		job.Input[k] = cloneValue(v)
	}
	if phone, ok := values["phone"].(string); ok && phone != "" {
		if phone != job.CurrentPhone {
			job.PhoneRetryRequired = false
		}
		job.CurrentPhone = phone
		job.PhoneError = ""
	}
	if value, ok := values["phone_otp"].(string); ok && value == "p" {
		job.CurrentPhone = ""
		job.PhoneRetryRequired = false
	}
	if v, ok := values["password"].(string); ok && v != "" {
		job.HasPassword = true
		job.LoginMode = "password"
	}
	if v, ok := values["totpSecret"].(string); ok && v != "" {
		job.HasTOTPKey = true
	}
	if _, active := m.active[id]; !active {
		job.Status = StatusQueued
		job.OperationStatus = "queued"
		job.QueuedAt = time.Now().UTC()
	} else if stage != "" {
		job.Status, job.Stage = "working", "working"
		job.OperationStatus = "running"
		job.Prompt = submittedInputPrompt(stage, values, job.CurrentPhone)
		if stage == "phone" || stage == "phone_otp" {
			job.PhoneError = ""
		}
	}
	job.UpdatedAt = time.Now().UTC()
	if _, active := m.active[id]; !active && !containsPending(m.pending, id) {
		m.pending = append(m.pending, job)
	}
	m.persistLocked()
	m.saveSecretsLocked(job, values)
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return true
}

func submittedInputPrompt(stage string, values map[string]any, phone string) string {
	value, _ := values[stage].(string)
	switch stage {
	case "password":
		return "正在验证账号密码"
	case "mfa_otp":
		return "正在验证 2FA 验证码"
	case "totp_setup_otp":
		return "正在激活新的 2FA"
	case "email_otp":
		if value == "r" {
			return "正在重新发送邮箱验证码"
		}
		return "正在验证邮箱验证码"
	case "phone":
		return "正在向 " + value + " 发送手机验证码"
	case "phone_otp":
		switch value {
		case "r":
			if phone != "" {
				return "正在向 " + phone + " 重新发送验证码"
			}
			return "正在重新发送手机验证码"
		case "p":
			return "正在返回手机号输入"
		default:
			return "正在验证手机验证码"
		}
	}
	return "正在处理输入"
}

func (m *Manager) saveSecretsLocked(j *Job, delta map[string]any) {
	if m.vault == nil || j == nil {
		return
	}
	c := credentials.Credentials{}
	if v, ok := delta["password"].(string); ok {
		c.Password = v
	}
	if v, ok := delta["totpSecret"].(string); ok {
		c.TOTPSecret = v
	}
	if v, ok := delta["proxyUrl"].(string); ok {
		c.ProxyURL = v
	}
	if v, ok := delta["mailApiUrl"].(string); ok {
		c.MailAPIURL = v
	}
	if v, ok := delta["mailRequestMethod"].(string); ok {
		c.MailMethod = v
	}
	if v, ok := delta["mailRequestBody"].(string); ok {
		c.MailBody = v
	}
	if v, ok := delta["mailRequestHeaders"].(map[string]string); ok {
		c.MailHeaders = v
	}
	if v, ok := delta["mailRequestHeaders"].(map[string]any); ok {
		c.MailHeaders = map[string]string{}
		for k, x := range v {
			if q, ok := x.(string); ok {
				c.MailHeaders[k] = q
			}
		}
	}
	hasInput := false
	for _, key := range []string{"password", "totpSecret", "proxyUrl", "mailApiUrl", "mailRequestMethod", "mailRequestBody", "mailRequestHeaders"} {
		if _, ok := delta[key]; ok {
			hasInput = true
		}
	}
	if !hasInput {
		return
	}
	old, loadErr := credentials.LoadContext(m.rootCtx, m.vault, j.Email)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		j.LastError = "无法读取系统凭据，未覆盖现有凭据"
		if m.logger != nil {
			m.logger.Error("secure credential load failed", "error", safeError(loadErr))
		}
		return
	}
	if loadErr == nil {
		c.TOTPResetPending = old.TOTPResetPending
		c.PendingTOTPSecret, c.PendingTOTPURI = old.PendingTOTPSecret, old.PendingTOTPURI
		c.PendingPassword = old.PendingPassword
		if _, present := delta["password"]; !present {
			c.Password = old.Password
		}
		if _, present := delta["totpSecret"]; !present {
			c.TOTPSecret = old.TOTPSecret
		}
		if old.TOTPResetPending || m.totpBlockedLocked(j.Email) {
			c.TOTPSecret = old.TOTPSecret
			c.TOTPResetPending = true
			quarantineTOTP(j)
		}
		if _, present := delta["proxyUrl"]; !present {
			c.ProxyURL = old.ProxyURL
		}
		if _, present := delta["mailApiUrl"]; !present {
			c.MailAPIURL = old.MailAPIURL
		}
		if _, present := delta["mailRequestMethod"]; !present {
			c.MailMethod = old.MailMethod
		}
		if _, present := delta["mailRequestBody"]; !present {
			c.MailBody = old.MailBody
		}
		if _, present := delta["mailRequestHeaders"]; !present {
			c.MailHeaders = old.MailHeaders
		}
	}
	email := j.Email
	// The React console includes proxyUrl:"" for new direct-connect jobs.
	// Do not create a keychain entry and encryption key for an entirely empty
	// credential record; a previously saved account still needs its explicit
	// clears applied without retaining the old password or proxy.
	empty := !c.TOTPResetPending && c.Password == "" && c.TOTPSecret == "" && c.ProxyURL == "" && c.MailAPIURL == "" && c.MailBody == "" && len(c.MailHeaders) == 0 && c.PendingPassword == "" && c.PendingTOTPSecret == "" && c.PendingTOTPURI == ""
	if empty && errors.Is(loadErr, os.ErrNotExist) {
		return
	}
	var saveErr error
	if empty {
		saveErr = credentials.DeleteContext(m.rootCtx, m.vault, email)
		if errors.Is(saveErr, os.ErrNotExist) {
			saveErr = nil
		}
	} else {
		saveErr = credentials.SaveContext(m.rootCtx, m.vault, email, c)
	}
	if err := saveErr; err != nil {
		j.LastError = "系统凭据保存失败，登录资料仅保留于内存"
		if m.logger == nil {
			return
		}
		// Do not downgrade to plaintext or put the credential material in the
		// job response. The failure is observable only as a safe operational log.
		m.logger.Error("secure credential save failed", "email", redactEmail(email), "error", safeError(err))
	}
}

func (m *Manager) Requeue(id, operation string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.rootCtx.Err() != nil {
		return nil, errors.New("job manager is shutting down")
	}
	j, ok := m.jobs[id]
	if !ok {
		return nil, osErrNotFound(id)
	}
	if err := m.checkTOTPActionLocked(j.Email, &Job{ID: j.ID, Operation: operation}); err != nil {
		return nil, err
	}
	if m.deleting[emailKey(j.Email)] || m.importing[emailKey(j.Email)] {
		return nil, errors.New("account is being deleted or restored")
	}
	if _, running := m.active[id]; running {
		return nil, fmt.Errorf("job %s is still running", id)
	}
	before := cloneJob(j)
	j.Operation = operation
	beginOperationState(j)
	j.ResetTOTP = resetOperation(operation)
	initializeTOTPWorkflow(j, nil)
	j.LastOperationType = operation
	j.LastOperationAt = time.Now().UTC()
	if operation == "relogin" {
		clearManualRepairState(j)
		j.CurrentPhone, j.PhoneError = "", ""
		j.PhoneRetryRequired = false
		j.TOTPSetupSecret, j.TOTPSetupURI, j.TOTPSetupError = "", "", ""
	}
	j.Status = StatusQueued
	j.QueuedAt = time.Now().UTC()
	j.Error, j.LastError = "", ""
	j.FinishedAt = nil
	j.UpdatedAt = time.Now().UTC()
	if err := m.commitAdmissionLocked(j, before, JobEvent{JobID: id, Status: StatusQueued, Stage: "queued", Prompt: j.Prompt, At: j.UpdatedAt}); err != nil {
		return nil, err
	}
	return cloneJob(j), nil
}

func clearManualRepairState(job *Job) {
	job.AutoRepairBlocked = false
	job.AutoRepairBlockedReason = ""
	job.AutoRepairPendingAccountIDs = nil
	job.AutoRepairBackend = ""
	job.AutoRepairLastError = ""
	job.SecurityCheckRequired = false
	job.PhoneError = ""
}

// QueueOperation atomically updates process-local inputs and schedules a
// follow-up operation. It prevents monitor repair from waking the scheduler
// between Input and Requeue.
func (m *Manager) QueueOperation(id, operation string, values map[string]any) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.queueOperationLocked(id, operation, values)
}

func (m *Manager) queueOperationLocked(id, operation string, values map[string]any) (*Job, error) {
	if m.closing || m.rootCtx.Err() != nil {
		return nil, errors.New("job manager is shutting down")
	}
	j, ok := m.jobs[id]
	if !ok {
		return nil, osErrNotFound(id)
	}
	method, err := AuthorizationMethodForValues(j.AuthorizationMethod, values)
	if err != nil {
		return nil, err
	}
	previousMethod, _ := NormalizeAuthorizationMethod(j.AuthorizationMethod)
	if method != previousMethod && (operation == "regenerate" || operation == "reauthorize" || operation == "refresh") {
		operation = "relogin"
	}
	if err := m.checkTOTPActionLocked(j.Email, &Job{ID: j.ID, Operation: operation}); err != nil {
		return nil, err
	}
	if resetOperation(operation) {
		if err := m.checkOtherTOTPResultsLocked(j.Email, j.ID); err != nil {
			return nil, err
		}
		for _, related := range m.jobs {
			if emailKey(related.Email) == emailKey(j.Email) && related.ID != id && (containsPending(m.pending, related.ID) || m.active[related.ID] != nil) {
				return nil, errors.New("同账号已有任务排队或运行，请结束后再重置 2FA")
			}
		}
	}
	if m.deleting[emailKey(j.Email)] || m.importing[emailKey(j.Email)] {
		return nil, errors.New("account is being deleted or restored")
	}
	if _, running := m.active[id]; running {
		return nil, fmt.Errorf("job %s is still running", id)
	}
	wasImported := j.Status == StatusImported
	if wasImported {
		var err error
		values, err = m.prepareImportedActionLocked(j, values)
		if err != nil {
			return nil, err
		}
	}
	before := cloneJob(j)
	if j.Input == nil {
		j.Input = map[string]any{}
	}
	j.AuthorizationMethod = method
	for k, v := range values {
		j.Input[k] = cloneValue(v)
	}
	if v, ok := values["proxyUrl"].(string); ok {
		j.ProxyConfigured = v != ""
	}
	if v, ok := values["password"].(string); ok {
		j.HasPassword = v != ""
	}
	if v, ok := values["totpSecret"].(string); ok {
		j.HasTOTPKey = v != ""
	}
	if v, ok := values["mailApiUrl"].(string); ok {
		j.MailAPIURL = v
	}
	j.Operation = operation
	beginOperationState(j)
	j.LastOperationType = operation
	j.SecurityActionReturnStatus = ""
	j.ResetTOTP = resetOperation(operation)
	initializeTOTPWorkflow(j, values)
	if isSuboperation(operation) {
		j.LogoutAllDevicesAfterTOTP, _ = values["logoutAllDevicesAfterTotp"].(bool)
		j.LogoutAllDevicesStatus, j.LogoutAllDevicesError = "not_requested", ""
		if j.LogoutAllDevicesAfterTOTP {
			j.LogoutAllDevicesStatus = "pending"
		}
		if j.ResetTOTP {
			j.TOTPResetStatus, j.TOTPResetPhase, j.TOTPResetError = "pending", "initial", ""
		}
	}
	if wasImported && isSuboperation(operation) {
		j.SecurityActionReturnStatus = StatusImported
	} else if j.Attempts == 0 && (operation == "relogin" || operation == "full") {
		j.LastOperationType = "initial_authorization"
	}
	j.LastOperationAt = time.Now().UTC()
	if operation == "relogin" {
		clearManualRepairState(j)
		j.CurrentPhone, j.PhoneError = "", ""
		j.PhoneRetryRequired = false
		j.TOTPSetupSecret, j.TOTPSetupURI, j.TOTPSetupError = "", "", ""
	}
	j.Status = StatusQueued
	j.QueuedAt = time.Now().UTC()
	j.Error, j.LastError = "", ""
	j.FinishedAt = nil
	j.UpdatedAt = time.Now().UTC()
	if err := m.commitAdmissionLocked(j, before, JobEvent{JobID: id, Status: StatusQueued, Stage: "queued", Prompt: j.Prompt, At: j.UpdatedAt}); err != nil {
		return nil, err
	}
	m.saveSecretsLocked(j, values)
	return cloneJob(j), nil
}

func containsPending(items []*Job, id string) bool {
	for _, j := range items {
		if j.ID == id {
			return true
		}
	}
	return false
}
func safeID(id string) bool {
	if len(id) < 1 || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func emailKey(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
func (m *Manager) Subscribe(buffer int) (<-chan JobEvent, func()) {
	if buffer < 1 {
		buffer = 16
	}
	ch := make(chan JobEvent, buffer)
	m.mu.Lock()
	m.events[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		if _, ok := m.events[ch]; ok {
			delete(m.events, ch)
			close(ch)
		}
		m.mu.Unlock()
	}
}
func (m *Manager) Emit(e JobEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	m.emitLocked(e)
	m.persistLocked()
}
func (m *Manager) emitLocked(e JobEvent) {
	m.publishEventLocked(m.applyEventLocked(e))
}

// applyEventLocked updates the account without notifying subscribers, allowing
// operation admission to persist the staged state before publishing it.
func (m *Manager) applyEventLocked(e JobEvent) JobEvent {
	e = SanitizeJobEvent(e)
	if job, ok := m.jobs[e.JobID]; ok {
		wasWaiting := operationStatusForEvent(job.Status) == "waiting"
		preserveSubmittedPrompt := job.Stage == "working" && e.Status == StatusRunning
		e = sanitizeEventForJob(e, job)
		if m.active[e.JobID] != nil && job.FinishedAt == nil && (e.Status == "succeeded" || e.Status == StatusSucceeded) {
			// Protocol completion precedes secure credential saving and may be
			// followed by workflow steps. Only run's terminal event can complete.
			e.Stage, e.Status, e.Prompt = "finalizing", StatusRunning, "正在保存授权结果"
		}
		// Only explicitly public metadata crosses the structured event boundary.
		// Credentials and one-time values are never copied from event Data.
		if status, ok := e.Data["mailStatus"].(string); ok {
			job.MailStatus = status
		}
		if message, ok := e.Data["mailApiError"].(string); ok {
			job.MailAPIError = message
		}
		if phone, ok := e.Data["currentPhone"].(string); ok {
			job.CurrentPhone = phone
		}
		if message, ok := e.Data["phoneError"].(string); ok {
			job.PhoneError = message
		}
		if required, ok := e.Data["securityCheckRequired"].(bool); ok {
			job.SecurityCheckRequired = required
		}
		if required, ok := e.Data["phoneRetryRequired"].(bool); ok {
			job.PhoneRetryRequired = required
			if required {
				// A deferred write is uncertain. Previously queued automatic
				// input must not survive into the fresh manual prompt.
				clearPhoneInputCandidates(job)
			}
		}
		if e.Stage == "phone_rejected" {
			// A configured phone can be used by the runner before ConsumeInput.
			// Rejection must not consume its old process-local copy again.
			clearPhoneInputCandidates(job)
		}
		if e.Stage == "phone_verified" {
			job.SMSStatus = "verified"
			job.PhoneError = ""
			job.SecurityCheckRequired = false
			job.PhoneRetryRequired = false
		}
		if e.Stage != "" {
			job.Stage = e.Stage
		}
		if e.Status != "" {
			delete(m.inputPreparing, e.JobID)
			job.Status = publicStatus(e.Stage, e.Status)
			// run already classified a terminal outcome. A completed account
			// state must not overwrite a failed/canceled security action.
			if job.FinishedAt == nil || job.OperationStatus == "" {
				if status := operationStatusForEvent(e.Status); status != "" {
					job.OperationStatus = status
				}
			}
		}
		if e.Prompt != "" {
			job.Prompt = e.Prompt
		} else if e.Status != "" && job.FinishedAt == nil && (wasWaiting || !preserveSubmittedPrompt) {
			job.Prompt = ""
		}
		// Keep a bounded, structured summary for the logs endpoint. Never copy
		// event data because it may contain OTPs or tokens.
		m.appendOperationLogLocked(job, e)
		job.UpdatedAt = e.At
	}
	return e
}

func (m *Manager) publishEventLocked(e JobEvent) {
	for ch := range m.events {
		select {
		case ch <- e:
		default:
		}
	}
}

func clearPhoneInputCandidates(job *Job) {
	for _, key := range []string{"phone", "phone_otp", "sms_otp", "code", "value"} {
		delete(job.Input, key)
	}
}

func isTransientStatus(status string) bool {
	switch status {
	case "interrupted", "awaiting_input", StatusRunning, StatusWaitingInput, "starting", "email_otp", "password", "mfa_otp", "totp", "phone", "phone_otp", "sms_otp", "totp_setup_otp", "password_add", "password_add_starting", "totp_starting", "refreshing", "finalizing", "working":
		return true
	}
	return false
}

// publicStatus keeps the legacy console state machine visible while allowing
// protocol flows to emit generic running/awaiting_input event statuses.
func publicStatus(stage, status string) string {
	switch status {
	case "awaiting_input", "waiting_input":
		switch stage {
		case "totp", "mfa_otp":
			return "mfa_otp"
		case "sms_otp", "phone_otp":
			return "phone_otp"
		case "email_otp":
			return "email_otp"
		case "phone":
			return "phone"
		case "password":
			return "password"
		case "totp_setup", "totp_setup_otp":
			return "totp_setup_otp"
		}
		return StatusWaitingInput
	case "succeeded":
		return StatusSucceeded
	case "failed", "canceled", "completed", "queued":
		return status
	case "running":
		switch stage {
		case "start", "starting":
			return "starting"
		case "totp_starting":
			return "totp_starting"
		case "password_add", "password_add_starting":
			return "password_add_starting"
		case "resume_available", "reauth_required", "refreshing", "finalizing":
			return stage
		}
		return "working"
	default:
		return status
	}
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.cancel()
	m.mu.Lock()
	m.closing = true
	for _, cancel := range m.active {
		cancel()
	}
	m.persistLocked()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.lastPersistError
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *Manager) removePendingLocked(id string) {
	for i, j := range m.pending {
		if j.ID == id {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			return
		}
	}
}
func (m *Manager) persistLocked() {
	if m.store == nil {
		return
	}
	data := make([]map[string]any, 0, len(m.jobs))
	for _, j := range m.jobs {
		data = append(data, jobMetadata(j))
	}
	// Write while holding the manager lock so a response cannot race a stale
	// asynchronous write and overwrite newer metadata during shutdown. The
	// store itself performs temp-file+fsync+rename atomically.
	m.lastPersistError = m.store.WriteJSON(context.Background(), "job-meta.json", map[string]any{"version": 1, "jobs": data, "updated_at": time.Now().UTC()})
	if m.lastPersistError != nil && m.logger != nil {
		m.logger.Error("job metadata save failed", "error", safeError(m.lastPersistError))
	}
}

func jobMetadata(j *Job) map[string]any {
	if j == nil {
		return map[string]any{}
	}
	j = SanitizeJobForPublic(j)

	m := map[string]any{"id": j.ID, "operation": j.Operation, "last_operation_type": j.Operation, "queued_at": j.QueuedAt, "logs": j.Logs, "mail_api_error": j.MailAPIError, "totp_setup_error": j.TOTPSetupError, "password_add_error": j.PasswordAddError, "last_auth_automated": j.LastAuthAutomated, "last_auth_automation_reason": j.LastAuthAutomationReason, "last_auth_automated_at": j.LastAuthAutomatedAt, "last_auth_requirements": j.LastAuthRequirements, "email": j.Email, "status": j.Status, "stage": j.Stage, "prompt": j.Prompt, "last_error": j.Error, "result_saved": j.ResultSaved, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "attempt": j.Attempts, "login_mode": j.LoginMode, "mail_status": j.MailStatus, "proxy_configured": j.ProxyConfigured, "has_password": j.HasPassword, "has_totp_key": j.HasTOTPKey, "totp_known_enabled": j.TOTPKnownEnabled, "current_phone": j.CurrentPhone, "phone_error": j.PhoneError, "phone_retry_required": j.PhoneRetryRequired, "sms_provider_id": j.SMSProviderID, "sms_provider_name": j.SMSProviderName, "sms_service_label": j.SMSServiceLabel, "sms_status": j.SMSStatus, "sms_error": j.SMSError, "security_check_required": j.SecurityCheckRequired, "auto_repair_blocked": j.AutoRepairBlocked, "auto_repair_blocked_reason": j.AutoRepairBlockedReason, "auto_repair_pending_account_ids": j.AutoRepairPendingAccountIDs, "auto_repair_backend": j.AutoRepairBackend, "auto_repair_last_error": j.AutoRepairLastError}
	m["plan_type"] = j.PlanType
	m["authorization_method"] = j.AuthorizationMethod
	m["operation_status"], m["operation_error"], m["log_entries"], m["credential_warning"] = j.OperationStatus, j.OperationError, j.LogEntries, j.LastError
	if !j.LastOperationAt.IsZero() {
		m["last_operation_at"] = j.LastOperationAt
	}
	if j.QueuedAt.IsZero() {
		m["queued_at"] = nil
	}
	if j.SecurityActionReturnStatus != "" {
		m["security_action_return_status"] = j.SecurityActionReturnStatus
	}
	for key, value := range map[string]any{
		"reset_totp": j.ResetTOTP, "totp_reset_status": j.TOTPResetStatus, "totp_reset_phase": j.TOTPResetPhase,
		"totp_reset_error": j.TOTPResetError, "totp_recovery_pending": j.TOTPRecoveryPending,
		"totp_credential_invalidated":   j.TOTPCredentialInvalidated,
		"logout_all_devices_after_totp": j.LogoutAllDevicesAfterTOTP, "logout_all_devices_status": j.LogoutAllDevicesStatus,
		"logout_all_devices_error": j.LogoutAllDevicesError,
		"relogin_after_totp":       j.ReloginAfterTOTP, "upload_after_totp": j.UploadAfterTOTP,
		"totp_workflow_status": j.TOTPWorkflowStatus, "totp_workflow_step": j.TOTPWorkflowStep,
		"totp_workflow_error": j.TOTPWorkflowError, "totp_relogin_status": j.TOTPReloginStatus,
		"totp_upload_status":   j.TOTPUploadStatus,
		"upload_after_relogin": j.UploadAfterRelogin, "relogin_authorization_saved": j.ReloginAuthorizationSaved,
		"relogin_upload_status": j.ReloginUploadStatus, "relogin_upload_error": j.ReloginUploadError,
	} {
		m[key] = value
	}
	if j.StartedAt != nil {
		m["started_at"] = j.StartedAt
	}
	if j.FinishedAt != nil {
		m["finished_at"] = j.FinishedAt
	}
	if j.PasswordAddedAt != nil {
		m["password_added_at"] = j.PasswordAddedAt
	}
	if j.LastOperationType != "" {
		m["last_operation_type"] = j.LastOperationType
	}
	if j.AutoRepairLastAttemptAt != nil {
		m["auto_repair_last_attempt_at"] = j.AutoRepairLastAttemptAt
	}
	if j.AutoRepairLastSuccessAt != nil {
		m["auto_repair_last_success_at"] = j.AutoRepairLastSuccessAt
	}
	return m
}
func cloneJob(j *Job) *Job {
	c := *j
	c.Logs = append([]string(nil), j.Logs...)
	c.LogEntries = append([]LogEntry(nil), j.LogEntries...)
	c.Input = cloneMap(j.Input)
	c.AutoRepairPendingAccountIDs = append([]int(nil), j.AutoRepairPendingAccountIDs...)
	c.LastAuthRequirements = make(map[string]bool, len(j.LastAuthRequirements))
	for k, v := range j.LastAuthRequirements {
		c.LastAuthRequirements[k] = v
	}
	copyTime := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		x := *t
		return &x
	}
	c.StartedAt, c.FinishedAt, c.PasswordAddedAt = copyTime(j.StartedAt), copyTime(j.FinishedAt), copyTime(j.PasswordAddedAt)
	c.AutoRepairLastAttemptAt, c.AutoRepairLastSuccessAt, c.LastAuthAutomatedAt = copyTime(j.AutoRepairLastAttemptAt), copyTime(j.AutoRepairLastSuccessAt), copyTime(j.LastAuthAutomatedAt)
	return &c
}
func cloneMap(x map[string]any) map[string]any {
	out := make(map[string]any, len(x))
	for k, v := range x {
		out[k] = cloneValue(v)
	}
	return out
}
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, v := range x {
			out[k] = v
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneValue(v)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	default:
		return v
	}
}
func (m *Manager) RecordAuthorization(id string, automated bool, reason string, requirements map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.LastAuthAutomated = automated
		j.AutoRepairBlocked = false
		j.AutoRepairBlockedReason = ""
		j.LastAuthAutomationReason = reason
		now := time.Now().UTC()
		j.LastAuthAutomatedAt = &now
		j.LastAuthRequirements = make(map[string]bool, len(requirements))
		for k, v := range requirements {
			j.LastAuthRequirements[k] = v
		}
		m.persistLocked()
	}
}
func newID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return "job-" + hex.EncodeToString(b)
}
func safeError(err error) string { return SafeJobError(err) }

func redactEmail(email string) string {
	email = strings.TrimSpace(email)
	if at := strings.LastIndexByte(email, '@'); at > 1 {
		return email[:1] + "***" + email[at:]
	}
	if len(email) > 2 {
		return email[:1] + "***"
	}
	return "***"
}
func osErrNotFound(id string) error { return fmt.Errorf("job %s not found", id) }
