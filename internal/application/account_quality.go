package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/sub2api"
)

// AccountQualityController connects explicit model generation to committed
// authorization and account ownership. It never logs in or rotates tokens.
type AccountQualityController struct {
	Service *accountquality.Service
	manual  *manualProbeState
	cfg     config.Config
	manager *jobs.Manager
	// A narrow transport seam allows deterministic protocol fault tests without
	// sending any request using real account data. Nil uses the configured TLS path.
	transportFactory func(string) (protocol.Doer, func(), error)
}

func NewAccountQualityController(cfg config.Config, manager *jobs.Manager) (*AccountQualityController, error) {
	c := &AccountQualityController{cfg: cfg, manager: manager, manual: newManualProbeState()}
	service, err := accountquality.New(cfg.OutputRoot, accountquality.Provider{Accounts: c.accounts, RetainedIDs: manager.RetainedAccountIDs, Acquire: c.acquire})
	if err != nil {
		return nil, err
	}
	c.Service = service
	return c, nil
}

func (c *AccountQualityController) accounts() []accountquality.Account {
	out := []accountquality.Account{}
	for _, j := range ReadOnlyAccountInventory(c.cfg.OutputRoot, c.manager) {
		a := accountquality.Account{ID: j.ID, Key: strings.ToLower(strings.TrimSpace(j.Email)), Revision: j.Revision, CanCheck: true}
		switch {
		case !j.ResultSaved:
			a.CanCheck = false
			a.Reason = "尚无已保存授权，检测不会自动登录"
		case j.RecoveryBlocked:
			a.CanCheck = false
			a.Reason = "安全操作待处理，暂不可检测"
		case j.Status == jobs.StatusQueued || j.Executing:
			a.CanCheck = false
			a.Reason = "账号操作进行中，检测暂缓"
		case !j.ExportRegular:
			a.CanCheck = false
			a.Reason = "已保存授权暂不可读取"
		}
		out = append(out, a)
	}
	return out
}

func qualityGrantError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, jobs.ErrAccountQueryBusy):
		return &accountquality.GateError{Code: "busy", Cause: accountquality.ErrBusy}
	case errors.Is(err, jobs.ErrAccountQuerySuperseded):
		return &accountquality.GateError{Code: "superseded", Cause: accountquality.ErrSuperseded}
	case errors.Is(err, jobs.ErrAccountQueryLockUnavailable):
		return &modelprobe.Error{Code: "ownership_unavailable"}
	case errors.Is(err, jobs.ErrTOTPRecoveryPending), errors.Is(err, jobs.ErrTOTPSetupVerificationPending), errors.Is(err, jobs.ErrTOTPRecoveryRequired), errors.Is(err, ErrInsightRecoveryPending):
		return &modelprobe.Error{Code: "recovery_pending"}
	case errors.Is(err, ErrInsightCredentialsUnavailable):
		return &modelprobe.Error{Code: "credentials_unavailable"}
	case errors.Is(err, ErrInsightGrantIdentity):
		return &modelprobe.Error{Code: "invalid_credentials"}
	default:
		return &modelprobe.Error{Code: "authorization_unavailable"}
	}
}

func (c *AccountQualityController) acquire(ctx context.Context, id string) (*accountquality.Lease, error) {
	g, err := LoadAccountInsightGrant(ctx, c.cfg.OutputRoot, c.manager, id)
	if err != nil {
		return nil, qualityGrantError(err)
	}
	run, closeTransport, err := c.probe(g)
	if err != nil {
		g.Release()
		return nil, err
	}
	return &accountquality.Lease{Context: g.Context(), Identity: c.qualityIdentity(g), Validate: g.Validate, Release: func() { closeTransport(); g.Release() }, Probe: run}, nil
}

// Include effective transport identity so restarting with another proxy or TLS
// configuration cannot reuse a measurement from a different network identity.
func (c *AccountQualityController) qualityIdentity(g *AccountInsightGrant) string {
	proxy := readOnlyProxy(c.cfg, g)
	profile := readOnlyTLSProfile(c.cfg)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s", g.IdentityKey, proxy, profile)))
	return hex.EncodeToString(sum[:])
}

func (c *AccountQualityController) probe(g *AccountInsightGrant) (func(context.Context, modelprobe.Options) (modelprobe.Result, error), func(), error) {
	recorder := beginGrantRequest(g, "candy")
	client, closeTransport, err := c.newProbeClient(g)
	if err != nil {
		completeProbeRequest(g, recorder, err)
		return nil, nil, err
	}
	return func(ctx context.Context, options modelprobe.Options) (modelprobe.Result, error) {
		if err := g.Validate(); err != nil {
			return modelprobe.Result{}, qualityGrantError(err)
		}
		result, err := client.Probe(ctx, g.Credentials, options)
		if ctx == nil || ctx.Err() == nil {
			completeProbeRequest(g, recorder, err)
		}
		return result, err
	}, closeTransport, nil
}

func (c *AccountQualityController) newProbeClient(g *AccountInsightGrant) (*modelprobe.Client, func(), error) {
	doer, closeTransport, err := openReadOnlyTransport(c.cfg, g, c.transportFactory)
	if err != nil {
		return nil, nil, &modelprobe.Error{Code: string(err.Code)}
	}
	return &modelprobe.Client{Transport: doer}, closeTransport, nil
}

func qualityUploadError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var gate *accountquality.GateError
	if errors.As(err, &gate) {
		switch gate.Code {
		case "degraded":
			return &sub2api.AuthorizationGateError{Reason: "degraded"}
		case "busy", "cooldown":
			return &sub2api.AuthorizationGateError{Reason: "busy"}
		case "settings_unavailable":
			return &sub2api.AuthorizationGateError{Reason: "settings"}
		case "persistence":
			return &sub2api.AuthorizationGateError{Reason: "persistence"}
		}
	}
	if errors.Is(err, accountquality.ErrSettingsUnavailable) || errors.Is(err, accountquality.ErrPersistenceUncertain) {
		return &sub2api.AuthorizationGateError{Reason: "settings"}
	}
	if errors.Is(err, accountquality.ErrCachePersistence) {
		return &sub2api.AuthorizationGateError{Reason: "persistence"}
	}
	return &sub2api.AuthorizationGateError{Reason: "failed"}
}

func qualityAccountMatches(g *AccountInsightGrant, account map[string]any) bool {
	creds, _ := account["credentials"].(map[string]any)
	access, _ := creds["access_token"].(string)
	if access != g.Credentials.AccessToken {
		return false
	}
	extra, _ := account["extra"].(map[string]any)
	method, err := jobs.AuthorizationMethodForClientIdentity(creds, extra)
	if err != nil || method != "" && method != g.Credentials.AuthorizationMethod {
		return false
	}
	if email := sub2api.AccountEmail(account); email != "" && !strings.EqualFold(email, g.Credentials.Email) {
		return false
	}
	id, ok := insightIdentityField([]map[string]any{creds, extra}, false, "chatgpt_account_id", "account_id")
	return ok && (id == "" || id == g.Credentials.AccountID)
}

func (c *AccountQualityController) enabledForUpload(ctx context.Context) (bool, error) {
	if c == nil || c.Service == nil {
		return false, &sub2api.AuthorizationGateError{Reason: "settings"}
	}
	settings, err := c.Service.SettingsContext(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		return false, &sub2api.AuthorizationGateError{Reason: "settings"}
	}
	return settings.Enabled && settings.BeforeUpload, nil
}

func (c *AccountQualityController) checkGrant(ctx context.Context, id string, g *AccountInsightGrant, account map[string]any) error {
	if g.ExpiresAt != nil && !time.Now().Before(*g.ExpiresAt) {
		return &sub2api.AuthorizationGateError{Reason: "authorization_expired"}
	}
	if !qualityAccountMatches(g, account) {
		return &sub2api.AuthorizationGateError{Reason: "identity"}
	}
	run, closeTransport, err := c.probe(g)
	if err != nil {
		// Unavailable transports and expired grants are real failed attempts;
		// policy decides whether they block, rather than mislabeling them degraded.
		run = func(context.Context, modelprobe.Options) (modelprobe.Result, error) { return modelprobe.Result{}, err }
	} else {
		defer closeTransport()
	}
	validate := func() error {
		if g.ExpiresAt != nil && !time.Now().Before(*g.ExpiresAt) {
			return &modelprobe.Error{Code: "authorization_expired"}
		}
		return g.Validate()
	}
	decision, checkErr := c.Service.CheckForUpload(ctx, id, accountquality.UploadProbe{Identity: c.qualityIdentity(g), Validate: validate, Run: run})
	if checkErr != nil {
		return qualityUploadError(checkErr)
	}
	if !decision.Allowed {
		return &sub2api.AuthorizationGateError{Reason: "failed"}
	}
	return validate()
}

// NewUploadSession holds manual/monitor account leases and OS locks through all
// downstream mutations. Starting a newer account operation cancels the entire
// HTTP session, but ownership remains held until the consumer finishes cleanup.
func (c *AccountQualityController) NewUploadSession(parent context.Context, ids []string) (context.Context, func(context.Context, map[string]any) error, func()) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	grants := map[string]*AccountInsightGrant{}
	leases := map[string]*jobs.AccountQueryLease{}
	stops := []func() bool{}
	gate := make(chan struct{}, 1)
	var closeOnce sync.Once
	check := func(callCtx context.Context, account map[string]any) error {
		if callCtx == nil {
			callCtx = context.Background()
		}
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		case <-callCtx.Done():
			return callCtx.Err()
		}
		defer func() { <-gate }()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := callCtx.Err(); err != nil {
			return err
		}
		checkCtx, stopCheck := context.WithCancel(ctx)
		stopCaller := context.AfterFunc(callCtx, stopCheck)
		defer func() { stopCaller(); stopCheck() }()
		enabled, err := c.enabledForUpload(checkCtx)
		if err != nil {
			return err
		}
		email := sub2api.AccountEmail(account)
		selected := map[string]bool{}
		for _, id := range ids {
			selected[id] = true
		}
		id, known := "", false
		for _, job := range c.manager.List() {
			if email == "" || !strings.EqualFold(strings.TrimSpace(job.Email), email) {
				continue
			}
			known = true
			if len(selected) > 0 && !selected[job.ID] {
				continue
			}
			if id != "" && id != job.ID && enabled {
				return &sub2api.AuthorizationGateError{Reason: "identity"}
			}
			if id == "" {
				id = job.ID
			}
		}
		if id == "" {
			if !enabled && !known {
				if email != "" {
					return c.manager.CheckTOTPExportEmailContext(checkCtx, email)
				}
				return nil
			}
			return &sub2api.AuthorizationGateError{Reason: "identity"}
		}
		lease := leases[email]
		if lease == nil {
			lease, err = c.manager.ReserveAccountQuery(ctx, id)
			if err != nil {
				return qualityUploadError(qualityGrantError(err))
			}
			leases[email] = lease
			stops = append(stops, context.AfterFunc(lease.Context(), cancel))
		}
		if !lease.Valid() {
			return jobs.ErrAccountQuerySuperseded
		}
		if err := c.manager.CheckTOTPExportContext(checkCtx, id); err != nil {
			return err
		}
		if !enabled {
			return ctx.Err()
		}
		g := grants[id]
		if g == nil {
			g, err = loadAccountInsightGrantWithLease(c.cfg.OutputRoot, c.manager, id, uploadGrantLease{lease})
			if err != nil {
				return qualityUploadError(qualityGrantError(err))
			}
			grants[id] = g
		}
		if err := c.checkGrant(checkCtx, id, g, account); err != nil {
			return err
		}
		return c.manager.CheckTOTPExportContext(checkCtx, id)
	}
	closeSession := func() {
		closeOnce.Do(func() {
			cancel()
			gate <- struct{}{}
			defer func() { <-gate }()
			for _, stop := range stops {
				stop()
			}
			// Query ownership closes its OS lock before waking the scheduler.
			for _, lease := range leases {
				lease.Release()
			}
		})
	}
	return ctx, check, closeSession
}

// Grants borrow session ownership; even a failed grant load must leave both
// locks held until NewUploadSession releases them in the required order.
type uploadGrantLease struct{ *jobs.AccountQueryLease }

func (uploadGrantLease) Release() {}

// WorkflowUploadCheck borrows an active runner's account instead of acquiring
// the read-query reservation that the runner itself intentionally excludes.
func (c *AccountQualityController) WorkflowUploadCheck(ctx context.Context, id string, account map[string]any) error {
	enabled, err := c.enabledForUpload(ctx)
	if err != nil || !enabled {
		return err
	}
	if !c.manager.ActiveAccountOperation(id) {
		return &sub2api.AuthorizationGateError{Reason: "busy"}
	}
	job, ok := c.manager.Get(id)
	if !ok {
		return &sub2api.AuthorizationGateError{Reason: "identity"}
	}
	values, ok := c.manager.InputCopy(id)
	if !ok {
		return &sub2api.AuthorizationGateError{Reason: "identity"}
	}
	proxyValue, override := values["proxyUrl"]
	proxy, valid := proxyValue.(string)
	if override && !valid {
		return &sub2api.AuthorizationGateError{Reason: "identity"}
	}
	lease := &workflowGrantLease{ctx: ctx, manager: c.manager, job: jobs.SanitizeJobForPublic(job), proxy: proxy, override: override}
	g, err := loadAccountInsightGrantWithLease(c.cfg.OutputRoot, c.manager, id, lease)
	if err != nil {
		return qualityUploadError(qualityGrantError(err))
	}
	defer g.Release()
	return c.checkGrant(ctx, id, g, account)
}

type workflowGrantLease struct {
	ctx      context.Context
	manager  *jobs.Manager
	job      *jobs.Job
	proxy    string
	override bool
}

func (l *workflowGrantLease) Context() context.Context   { return l.ctx }
func (l *workflowGrantLease) Job() *jobs.Job             { return jobs.SanitizeJobForPublic(l.job) }
func (l *workflowGrantLease) ProxyInput() (string, bool) { return l.proxy, l.override }
func (l *workflowGrantLease) Release()                   {}
func (l *workflowGrantLease) Valid() bool {
	if l.ctx.Err() != nil || !l.manager.ActiveAccountOperation(l.job.ID) {
		return false
	}
	j, ok := l.manager.Get(l.job.ID)
	if !ok {
		return false
	}
	values, ok := l.manager.InputCopy(l.job.ID)
	if !ok {
		return false
	}
	raw, override := values["proxyUrl"]
	proxy, valid := raw.(string)
	return j.Email == l.job.Email && j.CreatedAt.Equal(l.job.CreatedAt) && j.Attempts == l.job.Attempts && j.AuthorizationMethod == l.job.AuthorizationMethod && j.LastOperationAt.Equal(l.job.LastOperationAt) && j.ResultSaved == l.job.ResultSaved && j.ProxyConfigured == l.job.ProxyConfigured && override == l.override && (!override || valid && proxy == l.proxy)
}
