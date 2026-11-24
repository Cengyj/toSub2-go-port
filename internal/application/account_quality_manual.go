package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/modelprobe"
)

// ManualProbeRecord is a volatile visual sample. It is deliberately separate
// from the scored quality snapshots used by the authorization upload gate.
type ManualProbeRecord struct {
	ID              string     `json:"id"`
	RequestID       string     `json:"requestId,omitempty"`
	Task            string     `json:"task"`
	State           string     `json:"state"`
	Model           string     `json:"model,omitempty"`
	ReasoningEffort string     `json:"reasoningEffort,omitempty"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	NextAllowedAt   *time.Time `json:"nextAllowedAt,omitempty"`
	HTML            string     `json:"html,omitempty"`
	SVG             string     `json:"svg,omitempty"`
	ErrorCode       string     `json:"errorCode,omitempty"`
	HTTPStatus      int        `json:"httpStatus,omitempty"`
	Message         string     `json:"message,omitempty"`
	DurationMS      int64      `json:"durationMs,omitempty"`
}
type manualProbeAttempt struct {
	record          ManualProbeRecord
	cancel          context.CancelFunc
	cancelled       bool
	selection       string
	accountRevision string
}
type manualProbeState struct {
	mu                  sync.Mutex
	ctx                 context.Context
	cancel              context.CancelFunc
	records             map[string]*manualProbeAttempt
	seen                map[string]*manualProbeAttempt
	active              int
	observations        map[string]manualHTTPObservation
	observationRevision uint64
	done                chan struct{}
}

func newManualProbeState() *manualProbeState {
	return &manualProbeState{records: map[string]*manualProbeAttempt{}, seen: map[string]*manualProbeAttempt{}, observations: map[string]manualHTTPObservation{}}
}

func (c *AccountQualityController) StartManualProbes(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	m := c.manual
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil {
		return
	}
	m.ctx, m.cancel = context.WithCancel(parent)
}
func (c *AccountQualityController) CloseManualProbes(ctx context.Context) error {
	m := c.manual
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	done := m.done
	m.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *AccountQualityController) Status() accountquality.Status {
	status := c.Service.Status()
	m := c.manual
	m.mu.Lock()
	status.Active += m.active
	status.Revision += m.observationRevision
	m.mu.Unlock()
	return status
}

func (c *AccountQualityController) Activity() int {
	queued, active := c.Service.Activity()
	c.manual.mu.Lock()
	defer c.manual.mu.Unlock()
	return queued + active + c.manual.active
}
func validManualRequestID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func (c *AccountQualityController) ManualProbe(id string) ManualProbeRecord {
	m := c.manual
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := c.manager.Get(id); ok {
		if attempt := m.records[id]; attempt != nil {
			return attempt.record
		}
	}
	return ManualProbeRecord{ID: id, Task: modelprobe.PelicanTask, State: "idle"}
}
func (c *AccountQualityController) StartManualProbe(ctx context.Context, id, requestID, task string) (ManualProbeRecord, error) {
	return c.StartManualProbeWithOptions(ctx, id, requestID, task, nil)
}

func (c *AccountQualityController) StartManualProbeWithOptions(ctx context.Context, id, requestID, task string, selected *accountquality.RunOptions) (ManualProbeRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ManualProbeRecord{}, err
	}
	if !validManualRequestID(requestID) || task != modelprobe.PelicanTask {
		return ManualProbeRecord{}, &modelprobe.Error{Code: "invalid_options"}
	}
	settings, err := c.Service.SettingsContext(ctx)
	if err != nil {
		return ManualProbeRecord{}, err
	}
	if !settings.Enabled {
		return ManualProbeRecord{}, accountquality.ErrDisabled
	}
	selection := ""
	if selected != nil {
		copy := *selected
		selected = &copy
		selection = "selected|" + copy.Model + "|" + copy.ReasoningEffort
	}
	settings, err = accountquality.ApplyRunOptions(settings, selected)
	if err != nil {
		return ManualProbeRecord{}, err
	}
	if !c.Service.Status().Running {
		return ManualProbeRecord{}, accountquality.ErrClosed
	}
	m := c.manual
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil || m.ctx.Err() != nil {
		return ManualProbeRecord{}, accountquality.ErrClosed
	}
	// Request IDs prevent replays for 24 hours. Expired metadata is discarded;
	// an old drawing is released when this account starts a newer sample.
	for key, prior := range m.seen {
		if prior.record.FinishedAt != nil && time.Since(*prior.record.FinishedAt) > 24*time.Hour {
			delete(m.seen, key)
		}
	}
	key := id + "|" + requestID
	if prior := m.seen[key]; prior != nil {
		if prior.selection != selection {
			return ManualProbeRecord{}, accountquality.ErrConflict
		}
		return prior.record, nil
	}
	if m.active != 0 {
		return ManualProbeRecord{}, accountquality.ErrBusy
	}
	if prior := m.records[id]; prior != nil && prior.record.NextAllowedAt != nil && time.Now().Before(*prior.record.NextAllowedAt) {
		return ManualProbeRecord{}, &accountquality.GateError{Code: "cooldown"}
	}
	eligible := false
	accountRevision := ""
	for _, account := range c.accounts() {
		if account.ID == id && account.CanCheck {
			eligible = true
			accountRevision = account.Revision
			break
		}
	}
	if !eligible {
		return ManualProbeRecord{}, &modelprobe.Error{Code: "authorization_unavailable"}
	}
	grant, err := LoadAccountInsightGrant(m.ctx, c.cfg.OutputRoot, c.manager, id)
	if err != nil {
		return ManualProbeRecord{}, qualityGrantError(err)
	}
	recorder := beginGrantRequest(grant, "pelican")
	client, closeTransport, err := c.newProbeClient(grant)
	if err != nil {
		completeProbeRequest(grant, recorder, err)
		grant.Release()
		return ManualProbeRecord{}, err
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	stopGrant := context.AfterFunc(grant.Context(), cancel)
	now := time.Now().UTC()
	attempt := &manualProbeAttempt{record: ManualProbeRecord{ID: id, RequestID: requestID, Task: task, State: "running", Model: settings.Model, ReasoningEffort: settings.ReasoningEffort, StartedAt: &now}, cancel: cancel, selection: selection, accountRevision: accountRevision}
	if previous := m.records[id]; previous != nil && (previous.record.HTML != "" || previous.record.SVG != "") {
		previous.record.HTML, previous.record.SVG = "", ""
		previous.record.State, previous.record.ErrorCode, previous.record.Message = "failed", "result_replaced", "画面已被后一次手动检测替代；本请求未重复调用模型。"
	}
	m.records[id], m.seen[key], m.active, m.done = attempt, attempt, 1, make(chan struct{})
	done := m.done
	opts := modelprobe.Options{Model: settings.Model, ReasoningEffort: settings.ReasoningEffort, MinimumScore: settings.MinimumScore, TimeoutSeconds: settings.TimeoutSeconds}
	go func() {
		defer close(done)
		defer func() { cancel(); stopGrant(); closeTransport(); grant.Release() }()
		var result modelprobe.ManualResult
		probed := false
		// Match scored-probe isolation: an unexpected transport/parser panic
		// must still finalize the attempt and release its account reservation.
		err := func() (failure error) {
			defer func() {
				if recover() != nil {
					failure = errors.New("manual probe execution failed")
				}
			}()
			if failure = grant.Validate(); failure != nil {
				return failure
			}
			probed = runCtx.Err() == nil
			result, failure = client.Pelican(runCtx, grant.Credentials, opts)
			return failure
		}()
		if err == nil {
			err = grant.Validate()
		}
		if err == nil && grant.ExpiresAt != nil && !time.Now().Before(*grant.ExpiresAt) {
			err = &modelprobe.Error{Code: "authorization_expired"}
		}
		finished := time.Now().UTC()
		next := finished.Add(time.Duration(settings.CooldownSeconds) * time.Second)
		m.mu.Lock()
		attempt.record.FinishedAt, attempt.record.NextAllowedAt = &finished, &next
		attempt.record.DurationMS = finished.Sub(now).Milliseconds()
		if attempt.cancelled || errors.Is(err, context.Canceled) || runCtx.Err() != nil {
			attempt.record.State, attempt.record.ErrorCode, attempt.record.Message = "cancelled", "canceled", "手动检测已取消，没有生成评分。"
			attempt.record.HTTPStatus = modelprobe.DiagnoseFailure(err).HTTPStatus
		} else if err != nil {
			diagnostic := modelprobe.DiagnoseFailure(err)
			if diagnostic.Code == "probe_failed" {
				diagnostic.Code, diagnostic.Message = "invalid_response", "未取得可预览的完整 HTML 作品，请核对后手动重试。"
			}
			attempt.record.State, attempt.record.ErrorCode, attempt.record.Message = "failed", diagnostic.Code, diagnostic.Message
			attempt.record.HTTPStatus = diagnostic.HTTPStatus
			var gate *accountquality.GateError
			if errors.As(qualityGrantError(err), &gate) && gate.Code == "superseded" {
				attempt.record.ErrorCode = "superseded"
				attempt.record.Message = "账号授权已改变，本次画面未采用。"
			}
			if diagnostic.Code == "timeout" {
				attempt.record.Message = "绘图等待超时，未取得完整 HTML 作品；可手动重试。"
			}
		} else {
			attempt.record.State, attempt.record.HTML, attempt.record.SVG, attempt.record.Message = "success", result.HTML, result.SVG, "生成完成，请人工查看作品；该结果不计入自动探针或上传策略。"
		}
		m.observeHTTP(attempt)
		m.active = 0
		attempt.cancel = nil
		recordRequest := attempt.record.State != "cancelled" && attempt.record.ErrorCode != "superseded"
		historyID := sha256.Sum256([]byte(requestID + "|" + now.Format(time.RFC3339Nano)))
		history := accountquality.ProbeHistoryEntry{ID: "pelican-" + hex.EncodeToString(historyID[:]), Kind: "pelican", Status: attempt.record.State, Model: attempt.record.Model, FinishedAt: finished, ErrorCode: attempt.record.ErrorCode, HTTPStatus: attempt.record.HTTPStatus}
		m.mu.Unlock()
		if recordRequest {
			completeProbeRequest(grant, recorder, err)
		}
		if probed && recordRequest {
			historyCtx, cancelHistory := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelHistory()
			_ = c.Service.RecordProbeHistory(historyCtx, id, accountRevision, grant.Validate, history)
		}
	}()
	return attempt.record, nil
}

// CancelActiveManualProbe stops a visual request when the probe feature is
// explicitly disabled, without closing the lifecycle used after re-enabling.
func (c *AccountQualityController) CancelActiveManualProbe() {
	m := c.manual
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, attempt := range m.records {
		if attempt.cancel != nil {
			attempt.cancelled = true
			attempt.cancel()
		}
	}
}

func (c *AccountQualityController) CancelManualProbe(ctx context.Context, id, requestID string) (ManualProbeRecord, error) {
	if err := ctx.Err(); err != nil {
		return ManualProbeRecord{}, err
	}
	if strings.TrimSpace(requestID) == "" {
		return ManualProbeRecord{}, &modelprobe.Error{Code: "invalid_options"}
	}
	m := c.manual
	m.mu.Lock()
	defer m.mu.Unlock()
	attempt := m.seen[id+"|"+requestID]
	if attempt == nil {
		return ManualProbeRecord{}, accountquality.ErrSuperseded
	}
	if attempt.cancel != nil {
		attempt.cancelled = true
		attempt.cancel()
	}
	return attempt.record, nil
}
