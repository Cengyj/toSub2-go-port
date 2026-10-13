package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
	"tosub2-go-port/internal/sub2api"
	"tosub2-go-port/internal/transport"
)

type Server struct {
	cfg                  config.Config
	jobs                 *jobs.Manager
	logger               *slog.Logger
	static               fs.FS
	token                string
	mu                   sync.RWMutex
	mail                 mailRequestConfig
	monitorMu            sync.Mutex
	monitor              *sub2api.MonitorService
	monitorConfig        sub2api.Config
	monitorCancel        context.CancelFunc
	monitorNextCheckAt   *time.Time
	monitorDone          chan struct{}
	monitorWake          chan struct{}
	monitorVault         credentials.Store
	monitorKeyPersistent bool
	smsMu                sync.Mutex
	smsLeases            map[string]*smsLease
	smsAcquiring         map[string]bool
	smsPools             map[string]*providers.CustomPool
	smsCtx               context.Context
	smsStop              context.CancelFunc
	smsClosed            bool
	smsShutdownContext   context.Context
	smsWG                sync.WaitGroup
}

type mailRequestConfig struct {
	Method  string
	URL     string
	Body    string
	Headers map[string]string
}

func NewServer(cfg config.Config, jm *jobs.Manager, static embed.FS, logger *slog.Logger, vaults ...credentials.Store) *Server {
	sub, err := fs.Sub(static, "web/dist")
	if err != nil {
		sub = static
	}
	token := cfg.StartupToken
	if token == "" {
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			panic("console startup token could not be generated")
		}
		token = strings.TrimRight(base64URLEncode(buf), "=")
	}
	s := &Server{cfg: cfg, jobs: jm, static: sub, logger: logger, token: token, mail: mailRequestConfig{Method: http.MethodGet, Headers: map[string]string{}}}
	if len(vaults) > 0 {
		s.monitorVault = vaults[0]
	}
	s.initializeMonitor()
	s.initializeSMS()
	return s
}
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handle) }
func (s *Server) Close() {
	timeout := time.Duration(s.cfg.ShutdownTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = s.CloseContext(ctx)
}
func (s *Server) CloseContext(ctx context.Context) error {
	s.monitorMu.Lock()
	cancel, done := s.monitorCancel, s.monitorDone
	s.monitorCancel, s.monitorDone, s.monitorNextCheckAt = nil, nil, nil
	s.monitorMu.Unlock()
	if cancel != nil {
		cancel()
	}
	smsErr := s.closeSMSContext(ctx)
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return smsErr
}

func (s *Server) ListenAndServe() error {
	srv := &http.Server{Addr: s.cfg.Addr(), Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	return srv.ListenAndServe()
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if !s.validRequestOrigin(r) {
		writeError(w, http.StatusForbidden, "Invalid console origin or host")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	// Bootstrap is intentionally public so a local console can obtain its
	// per-process token. Every other API route requires the token.
	// Static assets are public so the first page can load bootstrap and obtain
	// its per-process token. Every protected API route still requires it.
	if strings.HasPrefix(r.URL.Path, "/api/") && !(r.Method == http.MethodGet && (r.URL.Path == "/api/bootstrap" || r.URL.Path == "/api/health")) && s.token != "" && !s.authorized(r) {
		writeError(w, http.StatusForbidden, "Invalid console token")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api(w, r)
		return
	}
	s.staticFile(w, r)
}
func (s *Server) authorized(r *http.Request) bool {
	supplied := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(supplied), "bearer ") {
		supplied = strings.TrimSpace(supplied[7:])
	}
	if supplied == "" {
		supplied = r.Header.Get("X-Console-Token")
	}
	if supplied == "" {
		supplied = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(s.token)) == 1
}
func (s *Server) staticFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "." || name == "" {
		name = "index.html"
	}
	file, err := s.static.Open(name)
	if err != nil {
		file, err = s.static.Open("index.html")
		name = "index.html"
	}
	if err != nil {
		http.Error(w, "static assets unavailable", 500)
		return
	}
	defer file.Close()
	if typ := mime.TypeByExtension(path.Ext(name)); typ != "" {
		w.Header().Set("Content-Type", typ)
	}
	if r.Method == http.MethodHead {
		return
	}
	io.Copy(w, file)
}
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	// Legacy provider option routes accept POST only. In particular, a GET or
	// OPTIONS request must not perform a provider request with a supplied body.
	if strings.HasPrefix(r.URL.Path, "/api/sms-providers/") && r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "unknown API route")
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/health":
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodGet && r.URL.Path == "/api/bootstrap":
		writeJSON(w, 200, map[string]any{"ok": true, "token": s.token, "host": s.cfg.Host, "port": s.cfg.Port, "maxActiveJobs": s.cfg.MaxActiveJobs, "tlsProfile": s.cfg.TLSProfile,
			"features": map[string]any{"retry": true, "regenerate": true, "phoneContext": true, "batchDownload": true, "bulkActions": true, "batchImportOnly": true, "pagination": true, "uniqueEmail": true, "queue": true, "sourceExport": true, "cancelAll": true, "sub2apiUpload": true, "sub2apiMonitor": true, "tlsFingerprint": true, "totpSetup": true, "totpReset": true, "logoutAllDevicesAfterTotp": true, "passwordAdd": true, "forceRelogin": true, "reloginAutoUpload": true, "accountInfo": true, "smsProviders": publicSMSProviderDefinitions()}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
		s.sendJobsPage(w, r)
	case r.Method == http.MethodPost && (r.URL.Path == "/api/jobs" || r.URL.Path == "/api/jobs/query"):
		s.createOrQuery(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/jobs/batch":
		s.createBatch(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/mail-request-config":
		s.mailRequestConfig(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/jobs/download-batch":
		s.downloadBatch(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/jobs/export-source":
		s.exportSource(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/jobs/cancel-all":
		n := s.jobs.CancelAll()
		for _, j := range s.jobs.List() {
			s.releaseSMS(j.ID)
		}
		writeJSON(w, 200, map[string]any{"canceled": n})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/jobs/") && strings.HasSuffix(r.URL.Path, "-batch"):
		s.batchAction(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "-batch"))
	case strings.HasPrefix(r.URL.Path, "/api/sub2api/"):
		s.sub2API(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/sms-providers/"):
		if !strings.HasSuffix(r.URL.Path, "/options") {
			writeError(w, 404, "unknown SMS provider route")
			return
		}
		providerID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sms-providers/"), "/options")
		var req struct {
			Config struct {
				APIKey  string `json:"apiKey"`
				Service string `json:"service"`
				BaseURL string `json:"baseUrl"`
			} `json:"config"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if providerID != "smsbower" {
			writeError(w, 400, "该接码平台不支持价格查询")
			return
		}
		if !validProviderAPIKey(req.Config.APIKey) {
			writeError(w, 400, "请输入有效的 SMSBower API Key")
			return
		}
		p := providers.NewSMSBowerProvider(req.Config.APIKey, envOr(req.Config.BaseURL, os.Getenv("SMSBOWER_API_BASE")), req.Config.Service, "")
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		opts, err := p.ListNumberOptions(ctx, req.Config.Service)
		if err != nil {
			writeError(w, 502, safeProviderError(err))
			return
		}
		writeJSON(w, 200, map[string]any{"providerId": providerID, "options": opts})
	case strings.HasPrefix(r.URL.Path, "/api/jobs/"):
		s.jobAction(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown API route")
	}
}

func publicSMSProviderDefinitions() []map[string]any {
	return []map[string]any{
		{"id": "luban", "name": "LubanSMS", "description": "使用供应商编号获取手机号", "fields": []map[string]any{{"key": "apiKey", "label": "API Key", "type": "password", "placeholder": "输入 LubanSMS API Key"}, {"key": "serviceId", "label": "供应商编号", "type": "text", "placeholder": "例如 121949", "summary": true}}},
		{"id": "smsbower", "name": "SMSBower", "description": "查询 OpenAI 实时价格和库存后选择国家", "optionsEndpoint": "/api/sms-providers/smsbower/options", "fields": []map[string]any{{"key": "apiKey", "label": "API Key", "type": "password", "placeholder": "输入 SMSBower API Key"}, {"key": "service", "label": "服务代码", "type": "hidden", "defaultValue": "dr"}, {"key": "country", "label": "国家与价格", "type": "price-select", "defaultValue": "1001", "summaryKey": "countryLabel"}, {"key": "maxPrice", "label": "最高价格", "type": "hidden", "required": false}, {"key": "countryLabel", "label": "国家名称", "type": "hidden", "required": false}}},
		{"id": "custom", "name": "自定义接码", "description": "批量粘贴手机号和对应的接码 API", "fields": []map[string]any{{"key": "entries", "label": "手机号与接码 API", "type": "textarea", "placeholder": "+861871291167----https://example.com/messages/1871291167"}}},
	}
}

func (s *Server) mailRequestConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config struct {
			Method, URL, Body string
			Headers           map[string]string
		} `json:"config"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	method := strings.ToUpper(strings.TrimSpace(body.Config.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		writeError(w, 400, "邮件请求方式只能是 GET 或 POST")
		return
	}
	if method == http.MethodPost && !isHTTPURL(body.Config.URL) {
		writeError(w, 400, "POST 模式必须填写有效的 HTTP 或 HTTPS 请求 URL")
		return
	}
	if len(body.Config.Headers) > 64 {
		writeError(w, 400, "请求头不能超过 64 个")
		return
	}
	for key := range body.Config.Headers {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "connection", "content-length", "host", "keep-alive", "proxy-authorization", "proxy-connection", "te", "trailer", "transfer-encoding", "upgrade":
			writeError(w, 400, "请求头包含禁止覆盖的传输字段")
			return
		}
	}
	s.mu.Lock()
	s.mail.Method = method
	s.mail.URL = body.Config.URL
	s.mail.Body = body.Config.Body
	s.mail.Headers = body.Config.Headers
	if s.mail.Headers == nil {
		s.mail.Headers = map[string]string{}
	}
	cfg := s.mail
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"config": map[string]any{"method": cfg.Method, "urlConfigured": cfg.URL != "", "headerCount": len(cfg.Headers)}})
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func (s *Server) publicJob(j *jobs.Job) map[string]any {
	if j == nil {
		return nil
	}
	autoEmailOTP := j.MailAPIURL != ""
	password, _ := j.Input["password"].(string)
	totp, _ := j.Input["totpSecret"].(string)
	j = jobs.SanitizeJobForPublic(j)
	autoRepairEligible, autoRepairReason := repairEligibility(j, password != "", autoEmailOTP, totp != "")
	canRetry := j.Status == jobs.StatusImported || j.Status == jobs.StatusFailed || j.Status == jobs.StatusCanceled || j.Status == "reauth_required" || j.Status == "resume_available"
	completed := j.Status == jobs.StatusSucceeded
	position := s.jobs.QueuePosition(j.ID)
	if j.Status == jobs.StatusQueued {
		position = max(position, 1)
		j.Prompt = fmt.Sprintf("排队中，前方还有 %d 条任务", position-1)
	}
	if j.LoginMode == "" {
		j.LoginMode = "email_otp"
	}
	operationAt := j.LastOperationAt
	if operationAt.IsZero() {
		operationAt = j.UpdatedAt
	}
	// Separate the latest action outcome from reusable authorization state.
	// For example, a canceled 2FA setup may leave an old output downloadable.
	p := s.publicTOTPState(j, map[string]any{"autoRepairEligible": autoRepairEligible, "autoRepairEligibilityReason": autoRepairReason, "queuePosition": position, "completedAt": j.FinishedAt, "restartRequired": j.Status == "reauth_required", "id": j.ID, "email": j.Email, "status": j.Status, "prompt": j.Prompt, "createdAt": j.CreatedAt, "updatedAt": j.UpdatedAt, "lastOperationAt": operationAt, "lastOperationType": publicOperationType(j), "lastError": firstNonEmpty(j.LastError, j.Error), "canDownload": j.ResultSaved, "planType": j.PlanType, "loginMode": j.LoginMode, "hasTotpKey": j.HasTOTPKey, "autoEmailOtp": autoEmailOTP, "mailStatus": j.MailStatus, "mailApiError": j.MailAPIError, "currentPhone": j.CurrentPhone, "phoneError": j.PhoneError, "phoneRetryRequired": j.PhoneRetryRequired, "totpSetupError": j.TOTPSetupError, "passwordAddError": j.PasswordAddError, "passwordAddedAt": j.PasswordAddedAt, "smsProviderId": j.SMSProviderID, "smsProviderName": j.SMSProviderName, "smsServiceLabel": j.SMSServiceLabel, "smsStatus": j.SMSStatus, "smsError": j.SMSError, "securityCheckRequired": j.SecurityCheckRequired, "canRetry": canRetry, "canResume": j.Status == "resume_available", "canRegenerate": (completed || (canRetry && j.Stage == "refresh_output_pending")) && j.ResultSaved, "canForceRelogin": completed || canRetry, "canSetupTotp": !j.HasTOTPKey && !j.TOTPKnownEnabled && s.canSecurityOperation(j), "canAddPassword": !j.HasPassword && s.canSecurityOperation(j), "proxyConfigured": j.ProxyConfigured, "autoRepairBlocked": j.AutoRepairBlocked, "autoRepairBlockedReason": j.AutoRepairBlockedReason, "autoRepairPendingAccountIds": j.AutoRepairPendingAccountIDs, "autoRepairLastAttemptAt": j.AutoRepairLastAttemptAt, "autoRepairLastSuccessAt": j.AutoRepairLastSuccessAt, "autoRepairLastError": j.AutoRepairLastError, "attempt": j.Attempts})
	p["stage"], p["startedAt"], p["operationStatus"] = j.Stage, j.StartedAt, j.OperationStatus
	p["authorizationMethod"], _ = jobs.NormalizeAuthorizationMethod(j.AuthorizationMethod)
	p["operationError"], p["credentialWarning"] = firstNonEmpty(j.OperationError, j.Error), j.LastError
	return publicReloginUpload(j, p)
}
func publicOperationType(j *jobs.Job) string {
	op := j.Operation
	if j.LastOperationType != "" {
		op = j.LastOperationType
	}
	switch op {
	case "regenerate", "refresh", "reauthorize":
		return "reauthorize"
	case "reset-2fa", "reset_2fa":
		return "reset_2fa"
	case "setup-2fa", "setup_2fa", "setup-totp", "totp_setup":
		return "setup_2fa"
	case "add-password", "add_password", "password_add":
		return "add_password"
	case "relogin":
		if len(j.AutoRepairPendingAccountIDs) > 0 {
			return "automatic_relogin"
		}
		return "relogin"
	case "", "full":
		if j.Attempts > 1 {
			return "reauthorize"
		}
		return "initial_authorization"
	default:
		return op
	}
}

func (s *Server) publicSelectionJob(j *jobs.Job) map[string]any {
	p := s.publicJob(j)
	for _, k := range []string{"prompt", "updatedAt", "lastError", "logs"} {
		delete(p, k)
	}
	return p
}
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Server) sendJobsPage(w http.ResponseWriter, r *http.Request) {
	s.syncOutputs(r.Context(), false)
	all := consoleJobs(s.jobs.List())
	totalAll := len(all)
	globalStats := jobStats(all)
	filter := strings.TrimSpace(r.URL.Query().Get("email"))
	requested := 0
	if filter != "" {
		emails := normalizeQueryEmails(strings.Split(filter, ","))
		requested = len(emails)
		all = filterJobs(all, emails)
	}
	page := 1
	if n, e := strconv.Atoi(r.URL.Query().Get("page")); e == nil && n > 0 {
		page = n
	}
	size := 20
	if n, e := strconv.Atoi(r.URL.Query().Get("pageSize")); e == nil && n > 0 && n <= 100 {
		size = n
	}
	pageLimit := (len(all) + size - 1) / size
	if pageLimit < 1 {
		pageLimit = 1
	}
	if page > pageLimit {
		page = pageLimit
	}
	start := (page - 1) * size
	end := start + size
	if end > len(all) {
		end = len(all)
	}
	jobsOut := make([]map[string]any, 0, len(all))
	for _, j := range all {
		jobsOut = append(jobsOut, s.publicJob(j))
	}
	selection := make([]map[string]any, 0, len(all))
	for _, j := range all {
		selection = append(selection, s.publicSelectionJob(j))
	}
	pages := (len(all) + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	writeJSON(w, 200, map[string]any{"jobs": jobsOut[start:end], "total": len(all), "page": page, "pageSize": size, "pages": pages, "pagination": map[string]any{"page": page, "pageSize": size, "total": len(all), "totalPages": pages, "totalAll": totalAll}, "stats": globalStats, "selection": selection, "filter": map[string]any{"active": filter != "", "requested": requested, "matched": len(all)}})
}

func normalizeQueryEmails(emails []string) []string {
	seen := make(map[string]bool, len(emails))
	unique := make([]string, 0, len(emails))
	for _, value := range emails {
		email := strings.ToLower(strings.TrimSpace(value))
		if !seen[email] {
			seen[email] = true
			unique = append(unique, email)
		}
	}
	return unique
}

// The legacy console shows the newest record for each email first, while
// the scheduler continues to use its independent FIFO queue.
func consoleJobs(all []*jobs.Job) []*jobs.Job {
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	seen := map[string]bool{}
	out := make([]*jobs.Job, 0, len(all))
	for _, job := range all {
		key := strings.ToLower(strings.TrimSpace(job.Email))
		if !seen[key] {
			seen[key] = true
			out = append(out, job)
		}
	}
	return out
}

func isActiveStatus(status string) bool {
	switch status {
	case jobs.StatusRunning, jobs.StatusWaitingInput, "starting", "email_otp", "password", "mfa_otp", "totp", "phone", "phone_otp", "sms_otp", "totp_setup", "totp_setup_otp", "password_add", "password_add_starting", "totp_starting", "refreshing", "finalizing", "working":
		return true
	}
	return false
}

func filterJobs(all []*jobs.Job, raw []string) []*jobs.Job {
	want := map[string]bool{}
	for _, e := range raw {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			want[e] = true
		}
	}
	if len(want) == 0 {
		return all
	}
	out := make([]*jobs.Job, 0, len(all))
	for _, j := range all {
		if want[strings.ToLower(strings.TrimSpace(j.Email))] {
			out = append(out, j)
		}
	}
	return out
}

func isEmail(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 254 || strings.ContainsAny(v, " \t\r\n") || strings.Count(v, "@") != 1 {
		return false
	}
	parts := strings.SplitN(v, "@", 2)
	local, domain := parts[0], parts[1]
	if len(local) == 0 || len(local) > 64 || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	if !regexp.MustCompile("(?i)^[A-Z0-9.!#$%&'*+/=?^_`{|}~-]+$").MatchString(local) {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") || !regexp.MustCompile(`(?i)^[A-Z0-9-]+$`).MatchString(label) {
			return false
		}
	}
	return regexp.MustCompile(`(?i)^(?:[A-Z]{2,63}|XN--[A-Z0-9-]{2,59})$`).MatchString(labels[len(labels)-1])
}

func isTOTPSecret(v string) bool {
	v = strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(v), " ", ""), "=", ""))
	if len(v) < 16 || len(v) > 128 {
		return false
	}
	for _, r := range v {
		if !((r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7')) {
			return false
		}
	}
	return true
}

func (s *Server) canSecurityOperation(j *jobs.Job) bool {
	if totpRecoveryBlocked(j) {
		return false
	}
	if j.Status == jobs.StatusImported {
		return true
	}
	if j.Status == jobs.StatusSucceeded && j.ResultSaved {
		return true
	}
	if j.Status == jobs.StatusQueued {
		return false
	}
	switch j.Status {
	case "phone", "phone_otp", "sms_otp", "resume_available", "reauth_required", jobs.StatusFailed, jobs.StatusCanceled:
		info, err := os.Stat(filepath.Join(s.cfg.OutputRoot, j.ID, "login-checkpoint.json"))
		return err == nil && info.Mode().IsRegular()
	}
	return false
}
func (s *Server) batchAction(w http.ResponseWriter, r *http.Request, action string) {
	switch action {
	case "delete", "relogin", "reauthorize", "setup-2fa", "add-password":
	default:
		writeError(w, http.StatusNotFound, "unknown batch operation")
		return
	}
	var req struct {
		IDs                 []string        `json:"ids"`
		JobIDs              []string        `json:"jobIds"`
		ProxyURL            *string         `json:"proxyUrl"`
		ResetTOTP           json.RawMessage `json:"resetTotp"`
		LogoutAllDevices    json.RawMessage `json:"logoutAllDevicesAfterTotp"`
		ReloginAfterTOTP    json.RawMessage `json:"reloginAfterTotp"`
		UploadAfterTOTP     json.RawMessage `json:"uploadAfterTotp"`
		UploadAfterRelogin  json.RawMessage `json:"uploadAfterRelogin"`
		Sub2APIConfig       json.RawMessage `json:"sub2apiConfig"`
		AuthorizationMethod json.RawMessage `json:"authorizationMethod"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	options := map[string]any{}
	for key, raw := range map[string]json.RawMessage{"resetTotp": req.ResetTOTP, "logoutAllDevicesAfterTotp": req.LogoutAllDevices, "reloginAfterTotp": req.ReloginAfterTOTP, "uploadAfterTotp": req.UploadAfterTOTP, "uploadAfterRelogin": req.UploadAfterRelogin, "sub2apiConfig": req.Sub2APIConfig, "authorizationMethod": req.AuthorizationMethod} {
		if len(raw) > 0 {
			var value any
			if json.Unmarshal(raw, &value) != nil {
				writeError(w, 400, "invalid JSON")
				return
			}
			options[key] = value
		}
	}
	operation, optionErr := normalizeTOTPAction(action, options)
	if optionErr != nil {
		writeError(w, 400, optionErr.Error())
		return
	}
	ids := req.IDs
	if len(ids) == 0 {
		ids = req.JobIDs
	}
	selectedJobs, ok := s.resolveSelectedJobs(w, ids)
	if !ok {
		return
	}
	if action == "delete" {
		emails := map[string]bool{}
		for _, job := range selectedJobs {
			emails[strings.ToLower(strings.TrimSpace(job.Email))] = true
		}
		for _, job := range s.jobs.List() {
			if emails[strings.ToLower(strings.TrimSpace(job.Email))] {
				s.releaseSMS(job.ID)
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		deleted, err := s.jobs.DeleteAccounts(ctx, ids)
		if err != nil {
			writeError(w, http.StatusConflict, "删除未完成，请等待任务清理后重试")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
		return
	}
	values := options
	if req.ProxyURL != nil {
		if _, err := transport.ParseProxy(*req.ProxyURL); err != nil {
			writeError(w, 400, "invalid proxy URL")
			return
		}
		values["proxyUrl"] = strings.TrimSpace(*req.ProxyURL)
	}
	if action == "reauthorize" {
		for _, job := range selectedJobs {
			flags := s.publicJob(job)
			if flags["canRegenerate"] != true && flags["canRetry"] != true {
				writeError(w, http.StatusConflict, "选中的任务仍在进行中或无法重新授权")
				return
			}
		}
	}
	out := []any{}
	for _, job := range selectedJobs {
		id := job.ID
		flags := s.publicJob(job)
		allowed := false
		switch action {
		case "relogin", "reauthorize":
			allowed = flags["canForceRelogin"] == true
		case "setup-2fa":
			allowed = flags["canSetupTotp"] == true
			if operation == "reset-2fa" {
				allowed = flags["canResetTotp"] == true
			}
		case "add-password":
			allowed = flags["canAddPassword"] == true
		}
		if !allowed {
			continue
		}
		var next *jobs.Job
		var err error
		method, _ := jobs.AuthorizationMethodForValues(job.AuthorizationMethod, values)
		previousMethod, _ := jobs.NormalizeAuthorizationMethod(job.AuthorizationMethod)
		if action == "reauthorize" && method != previousMethod {
			next, err = s.jobs.QueueOperation(id, "relogin", values)
		} else if action == "reauthorize" && job.Status != jobs.StatusSucceeded {
			next, err = s.jobs.RetryWithValues(id, values)
		} else if action == "setup-2fa" || action == "add-password" {
			s.releaseSMS(id)
			next, err = s.jobs.SwitchOperation(r.Context(), id, operation, values)
		} else {
			next, err = s.jobs.QueueOperation(id, action, values)
		}
		if err == nil {
			out = append(out, s.publicJob(next))
		}
	}
	if action != "delete" && len(out) == 0 {
		writeError(w, 409, "选中的任务当前均无法执行该操作")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "jobs": out, "started": len(out), "skipped": len(selectedJobs) - len(out), "deleted": len(out)})
}

// Resolve the full selection before any mutation or private export so stale
// cross-page selections cannot silently produce a partial result.
func (s *Server) resolveSelectedJobs(w http.ResponseWriter, ids []string) ([]*jobs.Job, bool) {
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "请至少选择一条任务")
		return nil, false
	}
	seen := map[string]bool{}
	unique := []string{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
		if len(seen) > 500 {
			writeError(w, http.StatusBadRequest, "一次最多操作500条任务")
			return nil, false
		}
	}
	selected := make([]*jobs.Job, 0, len(unique))
	for _, id := range unique {
		job, ok := s.jobs.Get(id)
		if !ok || !safePathID(id) {
			writeError(w, http.StatusNotFound, "部分任务不存在，请刷新页面后重试")
			return nil, false
		}
		selected = append(selected, job)
	}
	return selected, true
}

type createRequest struct {
	preserveExistingCredentials bool
	ID                          string          `json:"id"`
	Email                       string          `json:"email"`
	Emails                      []string        `json:"emails"`
	Operation                   string          `json:"operation"`
	Input                       map[string]any  `json:"input"`
	Jobs                        []createRequest `json:"jobs"`
	Text                        string          `json:"text,omitempty"`
	Password                    string          `json:"password,omitempty"`
	TOTPSecret                  string          `json:"totpSecret,omitempty"`
	Phone                       string          `json:"phone,omitempty"`
	MailAPIURL                  string          `json:"mailApiUrl,omitempty"`
	MailRequestBody             string          `json:"mailRequestBody,omitempty"`
	ProxyURL                    string          `json:"proxyUrl,omitempty"`
	AuthorizationMethod         json.RawMessage `json:"authorizationMethod,omitempty"`
	Page                        int             `json:"page,omitempty"`
	PageSize                    int             `json:"pageSize,omitempty"`
	provided                    map[string]bool
}

// Keep absent fields distinct from explicit empty strings so changing a job
// back to OTP login or a direct connection really clears the old credential.
func (r *createRequest) UnmarshalJSON(b []byte) error {
	type plain createRequest
	var value plain
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	*r = createRequest(value)
	r.provided = map[string]bool{}
	for key := range fields {
		r.provided[key] = true
	}
	return nil
}

func (r createRequest) inputValues() map[string]any {
	out := map[string]any{}
	for k, v := range r.Input {
		out[k] = v
	}
	if len(r.AuthorizationMethod) > 0 {
		var value any
		_ = json.Unmarshal(r.AuthorizationMethod, &value)
		out["authorizationMethod"] = value
	}
	if r.Password != "" || r.provided["password"] {
		out["password"] = r.Password
	}
	if r.TOTPSecret != "" || r.provided["totpSecret"] {
		out["totpSecret"] = r.TOTPSecret
	}
	if r.Phone != "" {
		out["phone"] = r.Phone
	}
	if r.MailAPIURL != "" || r.provided["mailApiUrl"] {
		out["mailApiUrl"] = r.MailAPIURL
	}
	if r.MailRequestBody != "" || r.provided["mailRequestBody"] {
		out["mailRequestBody"] = r.MailRequestBody
	}
	if r.ProxyURL != "" || r.provided["proxyUrl"] {
		out["proxyUrl"] = r.ProxyURL
	}
	return out
}

func validateAccountInput(values map[string]any) error {
	if _, err := jobs.AuthorizationMethodForValues("", values); err != nil {
		return err
	}
	for _, key := range []string{"password", "totpSecret", "proxyUrl", "mailApiUrl", "mailRequestBody"} {
		value, exists := values[key]
		if !exists {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("invalid %s field", key)
		}
		switch key {
		case "totpSecret":
			text = strings.ToUpper(strings.Join(strings.Fields(strings.ReplaceAll(text, "=", "")), ""))
			if text != "" && !isTOTPSecret(text) {
				return fmt.Errorf("TOTP 密钥必须为有效的 Base32 字符串")
			}
			values[key] = text
		case "proxyUrl":
			if _, err := transport.ParseProxy(text); err != nil {
				return fmt.Errorf("invalid proxy URL")
			}
			values[key] = strings.TrimSpace(text)
		case "mailApiUrl":
			if text != "" && !isHTTPURL(text) {
				return fmt.Errorf("邮箱 API 必须使用 HTTP 或 HTTPS URL")
			}
			values[key] = strings.TrimSpace(text)
		case "mailRequestBody":
			if len(text) > 65536 {
				return fmt.Errorf("POST 请求体不能超过64KB")
			}
		}
	}
	return nil
}

func (s *Server) applyMailConfig(values map[string]any) map[string]any {
	s.mu.RLock()
	cfg := s.mail
	s.mu.RUnlock()
	if cfg.URL == "" && len(cfg.Headers) == 0 && cfg.Body == "" {
		return values
	}
	if values == nil {
		values = map[string]any{}
	}
	if _, ok := values["mailApiUrl"]; !ok && cfg.URL != "" {
		values["mailApiUrl"] = cfg.URL
	}
	values["mailRequestMethod"] = cfg.Method
	if cfg.Body != "" {
		if _, exists := values["mailRequestBody"]; !exists {
			values["mailRequestBody"] = cfg.Body
		}
	}
	values["mailRequestHeaders"] = cfg.Headers
	return values
}

func (s *Server) createOrQuery(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if r.URL.Path == "/api/jobs/query" {
		if len(req.Emails) == 0 {
			writeError(w, 400, "请至少输入一个筛选邮箱")
			return
		}
		if len(req.Emails) > 500 {
			writeError(w, 400, "一次最多筛选 500 个邮箱")
			return
		}
		for i, email := range req.Emails {
			if !isEmail(email) {
				writeError(w, 400, fmt.Sprintf("第 %d 个筛选邮箱格式错误", i+1))
				return
			}
		}
		req.Emails = normalizeQueryEmails(req.Emails)
		s.syncOutputs(r.Context(), false)
		list := consoleJobs(s.jobs.List())
		totalAll := len(list)
		globalStats := jobStats(list)
		list = filterJobs(list, req.Emails)
		page, size := req.Page, req.PageSize
		if page < 1 {
			page = 1
		}
		if size < 1 || size > 100 {
			size = 20
		}
		pageLimit := (len(list) + size - 1) / size
		if pageLimit < 1 {
			pageLimit = 1
		}
		if page > pageLimit {
			page = pageLimit
		}
		start := (page - 1) * size
		end := start + size
		if end > len(list) {
			end = len(list)
		}
		visible := list[start:end]
		out := make([]map[string]any, 0, len(visible))
		for _, j := range visible {
			out = append(out, s.publicJob(j))
		}
		selection := make([]map[string]any, 0, len(list))
		for _, j := range list {
			selection = append(selection, s.publicSelectionJob(j))
		}
		pages := (len(list) + size - 1) / size
		if pages < 1 {
			pages = 1
		}
		writeJSON(w, 200, map[string]any{"jobs": out, "total": len(list), "page": page, "pageSize": size, "pages": pages, "pagination": map[string]any{"page": page, "pageSize": size, "total": len(list), "totalPages": pages, "totalAll": totalAll}, "selection": selection, "stats": globalStats, "filter": map[string]any{"active": true, "requested": len(req.Emails), "matched": len(list)}})
		return
	}
	if !isEmail(req.Email) {
		writeError(w, 400, "Please enter a valid email address")
		return
	}
	vals := s.applyMailConfig(req.inputValues())
	if err := validateAccountInput(vals); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	job, created, err := s.jobs.SubmitUniqueEmail(jobs.Spec{ID: req.ID, Email: strings.TrimSpace(req.Email), Operation: req.Operation, Input: vals})
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, map[string]any{"job": s.publicJob(job), "created": created, "updated": !created && len(vals) > 0})
}
func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	specs := req.Jobs
	if len(specs) == 0 && strings.TrimSpace(req.Text) != "" {
		s.mu.RLock()
		mailConfig := s.mail
		s.mu.RUnlock()
		var err error
		specs, err = parseBatchEntries(req.Text, req.Operation, req.inputValues(), mailConfig)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	if len(specs) == 0 && len(req.Emails) > 0 {
		for _, email := range req.Emails {
			specs = append(specs, createRequest{Email: email, Input: req.inputValues(), Operation: req.Operation})
		}
	}
	if len(specs) == 0 {
		specs = []createRequest{req}
	}
	if len(specs) > 500 {
		writeError(w, 400, "batch exceeds 500 jobs")
		return
	}
	out := make([]map[string]any, 0, len(specs))
	created, updated := 0, 0
	inputs := make([]map[string]any, len(specs))
	for index, item := range specs {
		if !isEmail(item.Email) {
			writeError(w, 400, "批量任务中包含无效邮箱")
			return
		}
		if item.ID != "" && !safePathID(item.ID) {
			writeError(w, 400, "invalid job id")
			return
		}
		inputs[index] = item.inputValues()
		if !item.preserveExistingCredentials {
			inputs[index] = s.applyMailConfig(inputs[index])
		}
		if err := validateAccountInput(inputs[index]); err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	for index, item := range specs {
		vals := inputs[index]
		j, isNew, err := s.jobs.SubmitUniqueEmail(jobs.Spec{ID: item.ID, Email: strings.TrimSpace(item.Email), Operation: item.Operation, Input: vals, ImportOnly: true})
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		out = append(out, s.publicJob(j))
		if isNew {
			created++
		} else {
			updated++
		}
	}
	writeJSON(w, 201, map[string]any{"jobs": out, "created": created, "updated": updated})
}
func (s *Server) jobAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, 404, "job not found")
		return
	}
	id := parts[0]
	if len(parts) > 2 || !safePathID(id) {
		writeError(w, 404, "unknown job route")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		j, ok := s.jobs.Get(id)
		if !ok {
			writeError(w, 404, "job not found")
			return
		}
		writeJSON(w, 200, s.publicJob(j))
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	expectedMethod := http.MethodPost
	if action == "logs" || action == "download" || action == "totp-setup" || action == "account-info" {
		expectedMethod = http.MethodGet
	}
	if r.Method != expectedMethod {
		w.Header().Set("Allow", expectedMethod)
		writeError(w, 405, "method not allowed")
		return
	}
	switch action {
	case "account-info":
		s.accountInfo(w, r, id)
	case "input":
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			return
		}
		inputStage, inputValue, inputAction := "", "", ""
		if actionName, _ := body["action"].(string); actionName != "" {
			job, exists := s.jobs.Get(id)
			if !exists {
				writeError(w, 404, "job not found")
				return
			}
			value, _ := body["value"].(string)
			if actionName != "password" {
				value = strings.TrimSpace(value)
			}
			stage := actionName
			switch actionName {
			case "resend", "resend_email":
				stage = "email_otp"
				value = "r"
			case "resend_phone":
				stage = "phone_otp"
				value = "r"
			case "change_phone":
				stage = "phone_otp"
				value = "p"
			}
			allowed := map[string]bool{"email_otp": true, "phone": true, "phone_otp": true, "password": true, "mfa_otp": true, "totp_setup_otp": true}
			if !allowed[stage] {
				writeError(w, 400, "Unsupported input action")
				return
			}
			if job.Status != stage {
				writeError(w, 409, "任务当前不在对应输入阶段")
				return
			}
			if stage == "password" && value == "" {
				writeError(w, 400, "密码不能为空")
				return
			}
			if (actionName == "email_otp" || actionName == "mfa_otp" || actionName == "totp_setup_otp") && !regexp.MustCompile(`^[0-9]{6}$`).MatchString(value) {
				writeError(w, 400, "验证码必须为6位数字")
				return
			}
			if actionName == "phone_otp" && !regexp.MustCompile(`^[0-9]{4,8}$`).MatchString(value) {
				writeError(w, 400, "手机验证码必须为4到8位数字")
				return
			}
			if stage == "phone" && !regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`).MatchString(value) {
				writeError(w, 400, "手机号须使用E.164格式")
				return
			}
			inputStage, inputValue, inputAction = stage, value, actionName
			body = map[string]any{stage: value}
		}
		if inputStage != "" {
			var prepare func()
			if inputStage == "phone" || inputStage == "phone_otp" {
				prepare = func() {
					if inputStage == "phone" {
						s.prepareSMSPhoneInput(id, inputValue)
					} else if inputAction == "change_phone" {
						s.releaseSMS(id)
					}
					if inputStage == "phone_otp" {
						s.pauseSMSSubmission(id, inputValue)
					}
				}
			}
			if !s.jobs.InputForStageWithPreparation(id, inputStage, body, prepare) {
				writeError(w, 409, "任务当前不在对应输入阶段")
				return
			}
		} else if !s.jobs.Input(id, body) {
			writeError(w, 404, "job not found")
			return
		}
		j, _ := s.jobs.Get(id)
		writeJSON(w, 200, map[string]any{"ok": true, "job": s.publicJob(j)})
	case "cancel":
		if _, ok := s.jobs.Get(id); !ok {
			writeError(w, 404, "job not found")
			return
		}
		if s.jobs.Cancel(id) {
			s.releaseSMS(id)
		}
		j, _ := s.jobs.Get(id)
		writeJSON(w, 200, map[string]any{"ok": true, "job": s.publicJob(j)})
	case "retry":
		if _, ok := s.jobs.Get(id); !ok {
			writeError(w, 404, "job not found")
			return
		}
		var body map[string]any
		if r.Body != nil && r.ContentLength != 0 {
			if !decodeJSON(w, r, &body) {
				return
			}
		}
		if err := validateAccountInput(body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		j, err := s.jobs.RetryWithValues(id, body)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "job": s.publicJob(j)})
	case "regenerate", "relogin", "setup-2fa", "add-password":
		var body map[string]any
		if r.Body != nil && r.ContentLength != 0 {
			if !decodeJSON(w, r, &body) {
				return
			}
		}
		operation, optionErr := normalizeTOTPAction(action, body)
		if optionErr != nil {
			writeError(w, 400, optionErr.Error())
			return
		}
		current, ok := s.jobs.Get(id)
		if !ok {
			writeError(w, 404, "job not found")
			return
		}
		flags := s.publicJob(current)
		key := map[string]string{"regenerate": "canRegenerate", "relogin": "canForceRelogin", "setup-2fa": "canSetupTotp", "add-password": "canAddPassword"}[action]
		if operation == "reset-2fa" {
			key = "canResetTotp"
		}
		if flags[key] != true {
			writeError(w, 409, "任务当前无法执行该操作")
			return
		}
		if proxy, exists := body["proxyUrl"]; exists {
			value, ok := proxy.(string)
			if !ok {
				writeError(w, 400, "invalid proxy URL")
				return
			}
			if _, err := transport.ParseProxy(value); err != nil {
				writeError(w, 400, "invalid proxy URL")
				return
			}
		}
		var next *jobs.Job
		var err error
		if action == "setup-2fa" || action == "add-password" {
			s.releaseSMS(id)
			next, err = s.jobs.SwitchOperation(r.Context(), id, operation, body)
		} else {
			next, err = s.jobs.QueueOperation(id, action, body)
		}
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "job": s.publicJob(next)})
	case "sms-number":
		s.acquireSMSNumber(w, r, id)
	case "totp-setup":
		j, ok := s.jobs.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		if j.Status != "totp_setup_otp" || j.TOTPSetupSecret == "" {
			writeError(w, http.StatusNotFound, "TOTP setup secret is unavailable")
			return
		}
		// This is a dedicated, token-authenticated one-time operation response;
		// the secret is excluded from job listings, metadata and logs.
		writeJSON(w, http.StatusOK, map[string]any{"secret": j.TOTPSetupSecret, "uri": j.TOTPSetupURI})
	case "download":
		s.downloadJob(w, r, id)
	case "luban-number":
		s.acquireSMSNumber(w, r, id)
	case "logs":
		j, ok := s.jobs.Get(id)
		if !ok {
			writeError(w, 404, "job not found")
			return
		}
		j = jobs.SanitizeJobForPublic(j)
		writeJSON(w, 200, map[string]any{"id": j.ID, "logs": strings.Join(j.Logs, "\n"), "entries": j.LogEntries})
	default:
		writeError(w, 404, "unknown job action")
	}
}

func envOr(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
func safeProviderError(err error) string {
	if err == nil {
		return ""
	}
	return "接码平台请求失败，请检查配置、余额及号码状态"
}

func validProviderAPIKey(key string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9._-]{8,256}$`).MatchString(strings.TrimSpace(key))
}

func groupIDsToInts(raw []any) []int {
	out := []int{}
	seen := map[int]bool{}
	for _, v := range raw {
		var n int
		switch x := v.(type) {
		case float64:
			n = int(x)
		case string:
			n, _ = strconv.Atoi(strings.TrimSpace(x))
		case int:
			n = x
		}
		if n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func modelWhitelist(v any) []string {
	var raw []string
	switch x := v.(type) {
	case string:
		for _, line := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
			if strings.TrimSpace(line) != "" {
				raw = append(raw, strings.TrimSpace(line))
			}
		}
	case []string:
		raw = append(raw, x...)
	case []any:
		for _, item := range x {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				raw = append(raw, strings.TrimSpace(s))
			}
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, s := range raw {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (s *Server) sub2API(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && !(r.Method == http.MethodGet && r.URL.Path == "/api/sub2api/monitor") {
		w.Header().Set("Allow", http.MethodPost)
		if r.URL.Path == "/api/sub2api/monitor" {
			w.Header().Set("Allow", "GET, POST")
		}
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Keep the legacy route surface stable. Network operations are deliberately
	// delegated to an explicit Sub2API integration; no secret is persisted here.
	var req struct {
		BaseURL               string          `json:"baseUrl"`
		AdminAPIKey           string          `json:"adminApiKey"`
		APIKey                string          `json:"apiKey"`
		Payload               any             `json:"payload"`
		IDs                   []string        `json:"ids"`
		GroupIDs              []any           `json:"groupIds"`
		ExcludedPlanTypes     *[]string       `json:"excludedPlanTypes"`
		OpenAIRequestTimezone json.RawMessage `json:"openaiRequestTimezone"`
		Enabled               *bool           `json:"enabled"`
		Config                *struct {
			BaseURL               string          `json:"baseUrl"`
			AdminAPIKey           string          `json:"adminApiKey"`
			APIKey                string          `json:"apiKey"`
			GroupIDs              []any           `json:"groupIds"`
			ExcludedPlanTypes     *[]string       `json:"excludedPlanTypes"`
			ProxyID               any             `json:"proxyId"`
			ModelWhitelist        any             `json:"modelWhitelist"`
			CodexFingerprintMode  string          `json:"codexFingerprintMode"`
			OpenAIRequestTimezone json.RawMessage `json:"openaiRequestTimezone"`
			Concurrency           any             `json:"concurrency"`
			LoadFactor            any             `json:"loadFactor"`
			Priority              any             `json:"priority"`
		} `json:"config"`
	}
	if r.Method == http.MethodPost && r.URL.Path != "/api/sub2api/monitor/check" {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	if req.Config != nil {
		if len(req.OpenAIRequestTimezone) == 0 {
			req.OpenAIRequestTimezone = req.Config.OpenAIRequestTimezone
		}
		if req.ExcludedPlanTypes == nil {
			req.ExcludedPlanTypes = req.Config.ExcludedPlanTypes
		}
		if len(req.GroupIDs) == 0 {
			req.GroupIDs = req.Config.GroupIDs
		}
		if req.BaseURL == "" {
			req.BaseURL = req.Config.BaseURL
		}
		if req.AdminAPIKey == "" {
			req.AdminAPIKey = req.Config.AdminAPIKey
		}
		if req.APIKey == "" {
			req.APIKey = req.Config.APIKey
		}
	}
	if req.Config != nil {
		mode := strings.ToLower(strings.TrimSpace(req.Config.CodexFingerprintMode))
		if mode == "" {
			mode = "session"
		}
		switch mode {
		case "off", "device", "session", "full":
			req.Config.CodexFingerprintMode = mode
		default:
			writeError(w, 400, "invalid Codex fingerprint mode")
			return
		}
	}
	key := req.AdminAPIKey
	if key == "" {
		key = req.APIKey
	}
	openaiRequestTimezone := ""
	if len(req.OpenAIRequestTimezone) != 0 {
		var value any
		if err := json.Unmarshal(req.OpenAIRequestTimezone, &value); err != nil {
			writeError(w, http.StatusBadRequest, "OpenAI 请求时区必须为文本")
			return
		}
		var ok bool
		openaiRequestTimezone, ok = value.(string)
		if !ok {
			writeError(w, http.StatusBadRequest, "OpenAI 请求时区必须为文本")
			return
		}
	}
	openaiRequestTimezone, err := sub2api.NormalizeOpenAIRequestTimezone(openaiRequestTimezone)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	client := sub2api.New(sub2api.Config{BaseURL: req.BaseURL, APIKey: key, GroupIDs: groupIDsToInts(req.GroupIDs), OpenAIRequestTimezone: openaiRequestTimezone, OnRetry: s.sub2APIRetryLogger("")})
	switch r.URL.Path {
	case "/api/sub2api/monitor":
		if r.Method == http.MethodGet {
			writeJSON(w, 200, s.publicMonitorState())
			return
		}
		s.monitorMu.Lock()
		cfg := s.monitorConfig
		s.monitorMu.Unlock()
		if req.BaseURL != "" && req.BaseURL != cfg.BaseURL {
			cfg.APIKey = ""
		}
		if req.BaseURL != "" {
			cfg.BaseURL = req.BaseURL
		}
		if key != "" {
			cfg.APIKey = key
		}
		if req.GroupIDs != nil {
			cfg.GroupIDs = groupIDsToInts(req.GroupIDs)
		}
		if req.ExcludedPlanTypes != nil {
			cfg.ExcludedPlanTypes = sub2api.NormalizeExcludedPlanTypes(*req.ExcludedPlanTypes)
		}
		if len(req.OpenAIRequestTimezone) != 0 {
			cfg.OpenAIRequestTimezone = openaiRequestTimezone
		}
		if req.Config != nil {
			cfg.CodexFingerprintMode = req.Config.CodexFingerprintMode
		}
		enabled := cfg.BaseURL != "" && cfg.APIKey != ""
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		if e := s.configureMonitorContext(r.Context(), cfg, enabled, key != ""); e != nil {
			writeError(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, s.publicMonitorState())
	case "/api/sub2api/groups":
		v, e := client.Groups(r.Context())
		if e != nil {
			writeError(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"groups": v})
	case "/api/sub2api/options":
		g, e := client.Groups(r.Context())
		if e != nil {
			writeError(w, 400, e.Error())
			return
		}
		p, e := client.Proxies(r.Context())
		if e != nil {
			writeError(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"groups": g, "proxies": p})
	case "/api/sub2api/upload":
		payload := req.Payload
		selectedCount, uploadedCount := 0, 0
		if payload == nil {
			// The React console sends {ids, config}; build the legacy batch
			// payload from each completed job's private export file.
			selected, ok := s.resolveSelectedJobs(w, req.IDs)
			if !ok {
				return
			}
			selectedCount = len(selected)
			accounts := []any{}
			for _, job := range selected {
				if isActiveStatus(job.Status) || job.Status == jobs.StatusQueued || job.TOTPWorkflowStatus == "running" || job.TOTPWorkflowStatus == "pending" || totpRecoveryBlocked(job) {
					writeError(w, http.StatusConflict, "请等待账号当前流程结束后再上传授权")
					return
				}
				if !job.ResultSaved {
					continue
				}
				doc, err := s.readJobExport(job.ID)
				if err != nil {
					writeError(w, http.StatusConflict, err.Error())
					return
				}
				accounts = append(accounts, doc.Accounts...)
				uploadedCount++
			}
			if len(accounts) == 0 {
				writeError(w, 409, "选中的任务里没有已完成的导入文件")
				return
			}
			for _, a := range accounts {
				if m, ok := a.(map[string]any); ok {
					if req.Config != nil && len(req.Config.GroupIDs) > 0 {
						m["group_ids"] = req.Config.GroupIDs
					}
					if req.Config != nil && req.Config.ProxyID != nil {
						m["proxy_id"] = req.Config.ProxyID
					}
					if req.Config != nil && req.Config.CodexFingerprintMode != "" {
						extra, _ := m["extra"].(map[string]any)
						if extra == nil {
							extra = map[string]any{}
							m["extra"] = extra
						}
						extra["codex_fingerprint_mode"] = req.Config.CodexFingerprintMode
					}
					if req.Config != nil && len(modelWhitelist(req.Config.ModelWhitelist)) > 0 {
						if creds, ok := m["credentials"].(map[string]any); ok {
							mm := map[string]any{}
							for _, model := range modelWhitelist(req.Config.ModelWhitelist) {
								mm[model] = model
							}
							creds["model_mapping"] = mm
						}
					}
					if req.Config != nil && req.Config.Concurrency != nil {
						m["concurrency"] = req.Config.Concurrency
					}
					if req.Config != nil && req.Config.LoadFactor != nil {
						m["load_factor"] = req.Config.LoadFactor
					}
					if req.Config != nil && req.Config.Priority != nil {
						m["priority"] = req.Config.Priority
					}
					m["status"] = "active"
					m["schedulable"] = true
				}
			}
			payload = map[string]any{"accounts": accounts}
		}
		v, e := client.Upload(r.Context(), payload)
		if e != nil {
			writeError(w, 400, e.Error())
			return
		}
		if req.Payload != nil {
			if m, ok := payload.(map[string]any); ok {
				if a, ok := m["accounts"].([]any); ok {
					selectedCount, uploadedCount = len(a), len(a)
				}
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true, "selected": selectedCount, "uploaded": uploadedCount, "skipped": selectedCount - uploadedCount, "groupIds": groupIDsToInts(req.GroupIDs), "result": v})
	case "/api/sub2api/monitor/check":
		if !s.monitor.Configuration().Enabled {
			writeError(w, 409, "请先启用 Sub2API 号池监控")
			return
		}
		result, e := s.monitorCheck(r.Context(), "manual")
		if e != nil {
			writeError(w, 502, sub2api.SafeError(e))
			return
		}
		state := s.publicMonitorState()
		state["ok"], state["result"] = true, result
		writeJSON(w, 200, state)
	default:
		writeError(w, 404, "unknown Sub2API route")
	}
}

func (s *Server) exportSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		JobIDs []string `json:"jobIds"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ids := req.IDs
	if len(ids) == 0 {
		ids = req.JobIDs
	}
	selected, ok := s.resolveSelectedJobs(w, ids)
	if !ok {
		return
	}
	lines := []string{}
	for _, j := range selected {
		if totpRecoveryBlocked(j) || (j.ResetTOTP && (j.Status == jobs.StatusQueued || isActiveStatus(j.Status))) {
			writeError(w, http.StatusConflict, "2FA 密钥正在修改或恢复未完成，暂不能导出账号资料")
			return
		}
		vals, _ := s.jobs.InputCopy(j.ID)
		pw, _ := vals["password"].(string)
		totp, _ := vals["totpSecret"].(string)
		mailURL, _ := vals["mailApiUrl"].(string)
		mailBody, _ := vals["mailRequestBody"].(string)
		if c, e := s.jobs.LoadCredentialsContext(r.Context(), j.ID); e == nil {
			if c.TOTPResetPending {
				writeError(w, http.StatusConflict, "2FA 重置恢复尚未完成")
				return
			}
			if c.Password != "" {
				pw = c.Password
			}
			if c.TOTPSecret != "" {
				totp = c.TOTPSecret
			}
			if mailURL == "" {
				mailURL = c.MailAPIURL
			}
			if mailBody == "" {
				mailBody = c.MailBody
			}
		} else if errors.Is(e, jobs.ErrTOTPRecoveryRequired) || j.ResetTOTP || totpRecoveryBlocked(j) {
			writeError(w, http.StatusConflict, "当前账号资料无法安全读取")
			return
		}
		if (j.HasPassword || j.LoginMode == "password") && pw == "" {
			writeError(w, http.StatusConflict, "密码未能从系统安全凭据存储读取，请重新导入账号资料")
			return
		}
		if j.HasTOTPKey && totp == "" {
			writeError(w, http.StatusConflict, "2FA密钥未能从系统安全凭据存储读取，请重新导入账号资料")
			return
		}
		if j.LoginMode == "manual" && pw == "" && mailBody == "" && mailURL == "" {
			writeError(w, http.StatusConflict, "旧版本任务未保存原始登录资料，请重新导入后再导出")
			return
		}
		// Original account lines retain the supplied mail request body even when
		// restored legacy records have no method or the global method changed.
		if mailBody != "" {
			parts := []string{j.Email}
			if pw != "" {
				parts = append(parts, pw)
			}
			parts = append(parts, mailBody)
			if totp != "" {
				parts = append(parts, totp)
			}
			lines = append(lines, strings.Join(parts, "----"))
			continue
		}
		if pw != "" {
			parts := []string{j.Email, pw}
			if mailURL != "" {
				parts = append(parts, mailURL)
			}
			if totp != "" {
				parts = append(parts, totp)
			}
			lines = append(lines, strings.Join(parts, "----"))
			continue
		}
		if mailURL != "" {
			line := j.Email + "----" + mailURL
			if totp != "" {
				line += "----" + totp
			}
			lines = append(lines, line)
			continue
		}
		if totp != "" {
			lines = append(lines, j.Email+"--------"+totp)
		} else {
			lines = append(lines, j.Email)
		}
	}
	payload := []byte("\ufeff" + strings.Join(lines, "\n") + "\n")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="chatgpt-account-source-`+strconv.Itoa(len(lines))+`-accounts.txt"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(payload)
}

func (s *Server) downloadJob(w http.ResponseWriter, r *http.Request, id string) {
	if !safePathID(id) {
		writeError(w, 400, "invalid job id")
		return
	}
	if _, ok := s.jobs.Get(id); !ok {
		writeError(w, 404, "job not found")
		return
	}
	job, _ := s.jobs.Get(id)
	name := filepath.Base(id + ".json")
	if job != nil {
		safeEmail := regexp.MustCompile(`[^a-zA-Z0-9@._+-]`).ReplaceAllString(job.Email, "_")
		if safeEmail != "" {
			name = safeEmail + "-sub2api-import-oauth-" + time.Now().UTC().Format("20060102-150405") + ".json"
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	for _, candidate := range []string{filepath.Join(s.cfg.OutputRoot, id, "sub2api-import-oauth.json"), filepath.Join(s.cfg.OutputRoot, id, "sub2api-data.json")} {
		if b, e := os.ReadFile(candidate); e == nil {
			w.WriteHeader(200)
			_, _ = w.Write(b)
			return
		}
	}
	writeError(w, http.StatusNotFound, "The sub2api import file is not ready")
}

func (s *Server) downloadBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	selected, ok := s.resolveSelectedJobs(w, req.IDs)
	if !ok {
		return
	}
	accounts := []any{}
	proxies := []any{}
	for _, job := range selected {
		if !job.ResultSaved {
			continue
		}
		doc, err := s.readJobExport(job.ID)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		accounts = append(accounts, doc.Accounts...)
		proxies = append(proxies, doc.Proxies...)
	}
	if len(accounts) == 0 {
		writeError(w, http.StatusConflict, "选中的任务里没有已完成的导入文件")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="sub2api-import-oauth-`+strconv.Itoa(len(accounts))+`-accounts-`+time.Now().UTC().Format("20060102-150405")+`.json"`)
	writeJSON(w, 200, map[string]any{"type": "sub2api-data", "version": 1, "exported_at": time.Now().UTC().Format(time.RFC3339Nano), "proxies": uniqueJSON(proxies), "accounts": accounts})
}

type jobExport struct {
	Type     string `json:"type"`
	Accounts []any  `json:"accounts"`
	Proxies  []any  `json:"proxies"`
}

func (s *Server) readJobExport(id string) (jobExport, error) {
	for _, name := range []string{"sub2api-import-oauth.json", "sub2api-data.json"} {
		data, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, id, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return jobExport{}, errors.New("选中任务的导入文件不存在或无法读取")
		}
		var doc jobExport
		if json.Unmarshal(data, &doc) != nil || doc.Type != "sub2api-data" || len(doc.Accounts) == 0 {
			return jobExport{}, errors.New("选中任务的导入文件格式不正确")
		}
		return doc, nil
	}
	return jobExport{}, errors.New("选中任务的导入文件不存在或无法读取")
}

func uniqueJSON(items []any) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(items))
	for _, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		k := string(b)
		if !seen[k] {
			seen[k] = true
			out = append(out, item)
		}
	}
	return out
}

func safePathID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
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

func base64URLEncode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	out.Grow((len(b)*8 + 5) / 6)
	var acc uint32
	bits := 0
	for _, v := range b {
		acc = (acc << 8) | uint32(v)
		bits += 8
		for bits >= 6 {
			bits -= 6
			out.WriteByte(alphabet[(acc>>bits)&63])
		}
	}
	if bits > 0 {
		out.WriteByte(alphabet[(acc<<(6-bits))&63])
	}
	return out.String()
}
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		writeError(w, 400, "invalid JSON")
		return false
	}
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	if err := dec.Decode(v); err != nil {
		writeError(w, 400, "invalid JSON")
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		writeError(w, 400, "request must contain one JSON value")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func jobStats(list []*jobs.Job) map[string]int {
	out := map[string]int{"active": 0, "queued": 0, "completed": 0, "available": 0}
	for _, j := range list {
		if j.ResultSaved && !j.TOTPRecoveryPending && !j.TOTPCredentialInvalidated {
			out["available"]++
		}
		switch {
		case isActiveStatus(j.Status):
			out["active"]++
		case j.Status == jobs.StatusQueued:
			out["queued"]++
		case j.Status == jobs.StatusSucceeded:
			out["completed"]++
		}
	}
	return out
}
