package console

import (
	"bytes"
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
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tosub2-go-port/internal/accountinsight"
	"tosub2-go-port/internal/application"

	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/providers"
	"tosub2-go-port/internal/recyclebin"
	"tosub2-go-port/internal/sub2api"
	"tosub2-go-port/internal/transport"
)

type Server struct {
	recycleBin           *recyclebin.Service
	trashActionGate      chan struct{}
	insights             *accountinsight.Service
	quality              *application.AccountQualityController
	cfg                  config.Config
	jobs                 *jobs.Manager
	logger               *slog.Logger
	static               fs.FS
	token                string
	mu                   sync.RWMutex
	mail                 mailRequestConfig
	poolMonitorMu        sync.Mutex
	poolMonitor          poolMonitorSettings
	poolMonitorError     string
	poolMonitorUncertain bool
	poolMonitorChecking  bool
	poolMonitorGate      atomic.Pointer[poolMonitorGate]
	poolMonitorWrite     func(context.Context, string, []byte) error
	monitorMu            sync.Mutex
	monitorClosed        bool
	monitorCheckActive   bool
	monitorUncertain     bool
	monitor              *sub2api.MonitorService
	monitorConfigure     func(*sub2api.Client, sub2api.MonitorConfig, sub2api.MonitorHooks) error
	monitorConfig        sub2api.Config
	monitorCancel        context.CancelFunc
	monitorNextCheckAt   *time.Time
	monitorDone          chan struct{}
	monitorWake          chan struct{}
	monitorVault         credentials.Store
	monitorKeyPersistent bool
	cpaMu                sync.Mutex
	cpa                  cpaSettingsState
	cpaMonitor           cpaMonitorRuntime
	smsMu                sync.Mutex
	smsLeases            map[string]*smsLease
	smsAcquiring         map[string]bool
	smsCompleting        map[string]bool
	smsTrashing          map[string]bool
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
	s.poolMonitorGate.Store(&poolMonitorGate{})
	s.initializeMonitor()
	s.initializeCPA()
	s.initializePoolMonitor()
	s.initializeSMS()
	s.initializeAccountInsights()
	s.initializeAccountQuality()
	s.initializeRecycleBin()
	return s
}
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handle) }
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
	// Static assets, bootstrap and health are public so the local console can
	// load and obtain its per-process token. Other API routes require it.
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
	if r.URL.Path == "/api/pool-monitor" || strings.HasPrefix(r.URL.Path, "/api/pool-monitor/") {
		s.poolMonitorAPI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/cpa/") {
		s.cpaAPI(w, r)
		return
	}
	if r.URL.Path == "/api/account-quality" || strings.HasPrefix(r.URL.Path, "/api/account-quality/") {
		s.accountQualityAPI(w, r)
		return
	}
	if r.URL.Path == "/api/account-trash" || strings.HasPrefix(r.URL.Path, "/api/account-trash/") {
		s.accountTrashAPI(w, r)
		return
	}
	if r.URL.Path == "/api/account-insights" || strings.HasPrefix(r.URL.Path, "/api/account-insights/") {
		s.accountInsightsAPI(w, r)
		return
	}
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
			"features": map[string]any{"accountSubscription": true, "accountQuality": s.quality != nil, "accountInsights": s.insights != nil, "accountTrash": s.recycleBin != nil, "retry": true, "regenerate": true, "phoneContext": true, "batchDownload": true, "bulkActions": true, "batchImportOnly": true, "pagination": true, "uniqueEmail": true, "queue": true, "sourceExport": true, "cancelAll": true, "sub2apiUpload": true, "cpaUpload": true, "sub2apiMonitor": true, "tlsFingerprint": true, "totpSetup": true, "totpReset": true, "logoutAllDevicesAfterTotp": true, "passwordAdd": true, "forceRelogin": true, "reloginAutoUpload": true, "accountInfo": true, "smsProviders": publicSMSProviderDefinitions()}})
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
		n, err := s.jobs.CancelAllWithError()
		for _, j := range s.jobs.List() {
			s.releaseSMS(j.ID)
		}
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "canceled": n, "uncertain": true, "error": err.Error()})
			return
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

var (
	accountEmailLocal = regexp.MustCompile("(?i)^[A-Z0-9.!#$%&'*+/=?^_`{|}~-]+$")
	accountEmailLabel = regexp.MustCompile(`(?i)^[A-Z0-9-]+$`)
	accountEmailTLD   = regexp.MustCompile(`(?i)^(?:[A-Z]{2,63}|XN--[A-Z0-9-]{2,59})$`)
)

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
	if !accountEmailLocal.MatchString(local) {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") || !accountEmailLabel.MatchString(label) {
			return false
		}
	}
	return accountEmailTLD.MatchString(labels[len(labels)-1])
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

type batchActionRequest struct {
	IDs                     []string        `json:"ids"`
	TOTPIntentID            json.RawMessage `json:"totpIntentId"`
	JobIDs                  []string        `json:"jobIds"`
	ProxyURL                *string         `json:"proxyUrl"`
	ResetTOTP               json.RawMessage `json:"resetTotp"`
	LogoutAllDevices        json.RawMessage `json:"logoutAllDevicesAfterTotp"`
	ReloginAfterTOTP        json.RawMessage `json:"reloginAfterTotp"`
	UploadAfterTOTP         json.RawMessage `json:"uploadAfterTotp"`
	UploadAfterRelogin      json.RawMessage `json:"uploadAfterRelogin"`
	Sub2APIConfig           json.RawMessage `json:"sub2apiConfig"`
	UploadTarget            json.RawMessage `json:"uploadTarget"`
	CPAConfig               json.RawMessage `json:"cpaConfig"`
	AuthorizationMethod     json.RawMessage `json:"authorizationMethod"`
	RemovedReauthentication json.RawMessage `json:"reauthenticate"`
}

func (r *batchActionRequest) UnmarshalJSON(b []byte) error {
	type plain batchActionRequest
	var value plain
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	if err := rejectAmbiguousRequestFieldAliases(fields); err != nil {
		return err
	}
	if err := rejectRemovedAuthenticationInputs(fields); err != nil {
		return err
	}
	*r = batchActionRequest(value)
	return nil
}

func (s *Server) batchAction(w http.ResponseWriter, r *http.Request, action string) {
	switch action {
	case "delete", "relogin", "reauthorize", "setup-2fa", "add-password":
	default:
		writeError(w, http.StatusNotFound, "unknown batch operation")
		return
	}
	var req batchActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Do not silently downgrade an old remote-recovery request into a new
	// batch operation, including when its removed flag is false or null.
	if len(req.RemovedReauthentication) > 0 {
		writeError(w, http.StatusBadRequest, "当前操作不支持恢复远端会话或重新验证身份")
		return
	}
	options := map[string]any{}
	for key, raw := range map[string]json.RawMessage{"totpIntentId": req.TOTPIntentID, "resetTotp": req.ResetTOTP, "logoutAllDevicesAfterTotp": req.LogoutAllDevices, "reloginAfterTotp": req.ReloginAfterTOTP, "uploadAfterTotp": req.UploadAfterTOTP, "uploadAfterRelogin": req.UploadAfterRelogin, "sub2apiConfig": req.Sub2APIConfig, "uploadTarget": req.UploadTarget, "cpaConfig": req.CPAConfig, "authorizationMethod": req.AuthorizationMethod} {
		if len(raw) > 0 {
			var value any
			if json.Unmarshal(raw, &value) != nil {
				writeError(w, 400, "invalid JSON")
				return
			}
			options[key] = value
		}
	}
	if err := s.prepareUploadInputs(options); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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
		s.moveAccountsToTrash(w, r, ids)
		return
	}
	values := options
	for _, job := range selectedJobs {
		if err := validateCPAOperationAccount(job, values); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
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
	out := []map[string]any{}
	failures := []batchSubmissionFailure{}
	skipped := 0
	for _, job := range selectedJobs {
		id := job.ID
		if operation == "reset-2fa" {
			if replay, ok, err := s.jobs.ReplayTOTPIntent(id, values); err != nil {
				failures = append(failures, batchSubmissionFailure{ID: id, Error: batchSubmissionMessage(err)})
				continue
			} else if ok {
				out = append(out, s.publicJob(replay))
				continue
			}
		}
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
			skipped++
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
		} else {
			failures = append(failures, batchSubmissionFailure{ID: id, Error: batchSubmissionMessage(err), Uncertain: errors.Is(err, jobs.ErrAdmissionUncertain)})
		}
	}
	writeBatchSubmissionResult(w, len(selectedJobs), skipped, out, failures)
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
	TimeZone                    string          `json:"timeZone,omitempty"`
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
	if err := rejectAmbiguousRequestFieldAliases(fields); err != nil {
		return err
	}
	if err := rejectRemovedAuthenticationInputs(fields); err != nil {
		return err
	}
	if err := rejectRemovedAuthenticationOperation(value.Operation); err != nil {
		return err
	}
	*r = createRequest(value)
	r.provided = map[string]bool{}
	for key := range fields {
		for _, name := range []string{"password", "totpSecret", "mailApiUrl", "mailRequestBody", "proxyUrl"} {
			if strings.EqualFold(key, name) {
				r.provided[name] = true
			}
		}
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
	if err := rejectRemovedAuthenticationInputs(values); err != nil {
		return err
	}
	if _, err := jobs.AuthorizationMethodForValues("", values); err != nil {
		return err
	}
	if value, present := values["manualExpiresAt"]; present {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("手动到期时间必须是日期时间字符串")
		}
		normalized, err := jobs.NormalizeExpiry(text)
		if err != nil {
			return err
		}
		values["manualExpiresAt"] = normalized
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
		specs, err = parseBatchEntriesAt(req.Text, req.Operation, req.inputValues(), mailConfig, req.TimeZone, time.Now())
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
			writeBatchImportResult(w, len(specs), created, updated, out, err)
			return
		}
		out = append(out, s.publicJob(j))
		if isNew {
			created++
		} else {
			updated++
		}
	}
	writeBatchImportResult(w, len(specs), created, updated, out, nil)
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
	case "refresh-subscription":
		s.refreshSubscription(w, r, id)
	case "expiration":
		s.setAccountExpiry(w, r, id)
	case "input":
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			return
		}
		if err := rejectRemovedAuthenticationInputs(body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
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
		canceled, err := s.jobs.CancelWithError(id)
		if canceled {
			s.releaseSMS(id)
		}
		j, _ := s.jobs.Get(id)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "job": s.publicJob(j), "uncertain": true, "error": err.Error()})
			return
		}
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
	case "regenerate", "relogin", "setup-2fa", "complete-2fa", "add-password":
		var body map[string]any
		if r.Body != nil && r.ContentLength != 0 {
			if !decodeJSON(w, r, &body) {
				return
			}
		}
		if err := s.prepareUploadInputs(body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		operation, optionErr := normalizeTOTPAction(action, body)
		if optionErr != nil {
			writeError(w, 400, optionErr.Error())
			return
		}
		if operation == "reset-2fa" || operation == "complete-2fa" {
			if replay, ok, err := s.jobs.ReplayTOTPIntent(id, body); err != nil {
				writeError(w, 400, err.Error())
				return
			} else if ok {
				writeJSON(w, 200, map[string]any{"ok": true, "job": s.publicJob(replay)})
				return
			}
		}
		current, ok := s.jobs.Get(id)
		if !ok {
			writeError(w, 404, "job not found")
			return
		}
		if err := validateCPAOperationAccount(current, body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		flags := s.publicJob(current)
		key := map[string]string{"regenerate": "canRegenerate", "relogin": "canForceRelogin", "setup-2fa": "canSetupTotp", "complete-2fa": "canCompleteTotp", "add-password": "canAddPassword"}[action]
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
		s.sendRetainedLogs(w, r, j)
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
		Payload               json.RawMessage `json:"payload"`
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
	var submittedPayload any
	if len(req.Payload) > 0 && !bytes.Equal(bytes.TrimSpace(req.Payload), []byte("null")) {
		decoder := json.NewDecoder(bytes.NewReader(req.Payload))
		decoder.UseNumber()
		if err := decoder.Decode(&submittedPayload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid upload payload")
			return
		}
	}
	if r.URL.Path == "/api/sub2api/upload" && submittedPayload != nil {
		if authmaterial.ContainsLoginSessionJSON(req.Payload) {
			writeError(w, http.StatusBadRequest, "上传资料含已移除的登录会话材料")
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
	var uploadOptions sub2api.UploadOptions
	if r.URL.Path == "/api/sub2api/upload" && submittedPayload == nil {
		// Browser preferences contain text values, including blank numeric
		// defaults. Parse them exactly as automatic uploads do before lookup.
		settings := map[string]any{
			"baseUrl": req.BaseURL, "adminApiKey": key,
			"groupIds": req.GroupIDs, "openaiRequestTimezone": openaiRequestTimezone,
		}
		if req.Config != nil {
			settings["proxyId"] = req.Config.ProxyID
			settings["concurrency"] = req.Config.Concurrency
			settings["loadFactor"] = req.Config.LoadFactor
			settings["priority"] = req.Config.Priority
			settings["modelWhitelist"] = req.Config.ModelWhitelist
			settings["codexFingerprintMode"] = req.Config.CodexFingerprintMode
		}
		parsed, options, parseErr := sub2api.ParseUploadSettings(settings)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr.Error())
			return
		}
		req.BaseURL, key, uploadOptions = parsed.BaseURL, parsed.APIKey, options
	}
	var upstreamTimeout time.Duration
	if r.URL.Path == "/api/sub2api/groups" || r.URL.Path == "/api/sub2api/options" {
		// Foreground metadata reads retain their 120-second upstream window.
		// Bound the whole handler as well, including retries; options performs
		// two sequential reads. Background work and uploads keep their defaults.
		upstreamTimeout = 120 * time.Second
		budget := upstreamTimeout
		if r.URL.Path == "/api/sub2api/options" {
			budget *= 2
		}
		ctx, cancel := context.WithTimeout(r.Context(), budget)
		defer cancel()
		r = r.WithContext(ctx)
	}
	clientConfig := sub2api.Config{BaseURL: req.BaseURL, APIKey: key, GroupIDs: groupIDsToInts(req.GroupIDs), OpenAIRequestTimezone: openaiRequestTimezone, Timeout: upstreamTimeout, OnRetry: s.sub2APIRetryLogger("")}
	preparedUploads := map[string][]preparedSub2APIUpload{}
	if r.URL.Path == "/api/sub2api/upload" {
		ctx, check, release := s.quality.NewUploadSession(r.Context(), req.IDs)
		defer release()
		r = r.WithContext(ctx)
		clientConfig.BeforeAuthorizationSync = func(ctx context.Context, account map[string]any) error {
			checkRecovery := func() error {
				for _, id := range req.IDs {
					if err := s.jobs.CheckTOTPExport(id); err != nil {
						return err
					}
				}
				if email := sub2api.AccountEmail(account); email != "" {
					return s.jobs.CheckTOTPExportEmail(email)
				}
				return nil
			}
			if err := checkRecovery(); err != nil {
				return err
			}
			if err := check(ctx, account); err != nil {
				return err
			}
			if err := s.checkPreparedSub2APIUploads(ctx, preparedUploads[sub2api.AccountEmail(account)]); err != nil {
				return err
			}
			return checkRecovery()
		}
	}
	client := sub2api.New(clientConfig)
	switch r.URL.Path {
	case "/api/sub2api/monitor":
		if r.Method == http.MethodGet {
			writeJSON(w, 200, s.publicMonitorState())
			return
		}
		if s.poolMonitorGate.Load() != nil {
			s.poolMonitorMu.Lock()
			defer s.poolMonitorMu.Unlock()
			if s.poolMonitorChecking {
				writeError(w, http.StatusConflict, "巡检正在进行，请等待结束后保存服务配置")
				return
			}
			if req.Enabled != nil || req.ExcludedPlanTypes != nil {
				writeError(w, http.StatusConflict, "巡检规则已统一，请在号池监控中选择目标并保存")
				return
			}
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
		// Service connection edits must not silently enable or disable repair.
		// Only an explicit monitor policy update changes the saved switch.
		enabled := s.monitor.Configuration().Enabled
		if s.poolMonitorGate.Load() != nil {
			enabled = s.poolMonitorAllows("sub2api")
			cfg.ExcludedPlanTypes = append([]string{}, s.poolMonitor.ExcludedPlanTypes...)
		}
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		if e := s.configureMonitorContext(r.Context(), cfg, enabled, key != ""); e != nil {
			writeMonitorConfigurationError(w, e)
			return
		}
		if s.poolMonitorGate.Load() != nil && s.poolMonitor.Target == "sub2api" {
			// A confirmed connection save may recover a prior connection-only
			// pause. Shared-file uncertainty still requires its own explicit save.
			if e := s.applyPoolMonitorLocked(); e != nil {
				s.poolMonitorError = "服务配置已保存但巡检运行策略未确认，请重新读取后保存号池监控"
				s.poolMonitorGate.Store(&poolMonitorGate{})
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": s.poolMonitorError, "outcome": "unknown", "uncertain": true})
				return
			}
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
		payload := submittedPayload
		var repairUploads []*jobs.Job
		selectedCount, uploadedCount := 0, 0
		if payload == nil {
			// The React console sends {ids, config}; build the legacy batch
			// payload from each completed job's private export file.
			selected, ok := s.resolveSelectedJobs(w, req.IDs)
			if !ok {
				return
			}
			selectedCount = len(selected)
			accounts := []map[string]any{}
			for _, job := range selected {
				if err := s.jobs.CheckTOTPExport(job.ID); err != nil {
					writeError(w, http.StatusConflict, err.Error())
					return
				}
				if isActiveStatus(job.Status) || job.Status == jobs.StatusQueued || job.TOTPWorkflowStatus == "running" || job.TOTPWorkflowStatus == "pending" || totpRecoveryBlocked(job) {
					writeError(w, http.StatusConflict, "请等待账号当前流程结束后再上传授权")
					return
				}
				if !job.ResultSaved {
					continue
				}
				data, doc, name, err := s.readCommittedUpload(r.Context(), job)
				if err != nil {
					writeError(w, http.StatusConflict, err.Error())
					return
				}
				for _, value := range doc.Accounts {
					account, ok := value.(map[string]any)
					if !ok {
						writeError(w, http.StatusConflict, "选中任务的导入文件格式不正确")
						return
					}
					accounts = append(accounts, account)
					email := sub2api.AccountEmail(account)
					preparedUploads[email] = append(preparedUploads[email], preparedSub2APIUpload{id: job.ID, name: name, data: data})
				}
				if len(job.AutoRepairPendingAccountIDs) > 0 && (job.AutoRepairBackend == sub2api.BackendIdentity(req.BaseURL) || job.AutoRepairBackend == req.BaseURL) {
					repairUploads = append(repairUploads, job)
				}
				uploadedCount++
			}
			if len(accounts) == 0 {
				writeError(w, 409, "选中的任务里没有已完成的导入文件")
				return
			}
			payload, err = sub2api.BuildUploadPayload(accounts, uploadOptions)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if submittedPayload != nil {
			if m, ok := payload.(map[string]any); ok {
				if a, ok := m["accounts"].([]any); ok {
					selectedCount, uploadedCount = len(a), len(a)
				}
			}
		}
		v, e := client.Upload(r.Context(), payload)
		if e != nil {
			result := sub2api.PublicUploadResult(v)
			confirmedWrites := false
			for _, key := range []string{"created", "updated"} {
				if count, _ := result[key].(float64); count > 0 {
					confirmedWrites = true
				}
			}
			if !confirmedWrites && (errors.Is(e, jobs.ErrTOTPRecoveryRequired) || errors.Is(e, jobs.ErrTOTPExportUnavailable)) {
				message := jobs.ErrTOTPRecoveryRequired.Error()
				if errors.Is(e, jobs.ErrTOTPExportUnavailable) {
					message = jobs.ErrTOTPExportUnavailable.Error()
				}
				writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": message, "outcome": "blocked", "uncertain": false, "selected": selectedCount, "uploaded": 0, "skipped": selectedCount, "result": map[string]any{"success": 0, "created": 0, "updated": 0}})
				return
			}
			var qualityGate *sub2api.AuthorizationGateError
			if !confirmedWrites && errors.As(e, &qualityGate) {
				// Preflight checks the whole selected batch before any Sub2 write.
				// Do not present eligible-file counts as attempted uploads.
				writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": sub2api.SafeError(e), "outcome": "blocked", "qualityBlocked": qualityGate.Reason != "authorization_changed", "uncertain": false, "selected": selectedCount, "uploaded": 0, "skipped": selectedCount, "result": map[string]any{"success": 0, "created": 0, "updated": 0}})
				return
			}
			// HTTP success from the upstream does not prove that a mutation
			// succeeded. Preserve confirmed progress and the uncertainty class
			// so the browser never invites replay of an unconfirmed creation.
			outcome := "failed"
			if sub2api.IsMutationOutcomeUnknown(e) {
				outcome = "unknown"
			}
			// Updated credentials remain committed even when clearing a prior
			// error or restoring scheduling fails. Complete success stays zero
			// in that case, but the browser must still show partial progress.
			for _, key := range []string{"success", "updated", "created"} {
				if confirmed, _ := result[key].(float64); confirmed > 0 {
					outcome = "partial"
					break
				}
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"ok": false, "error": sub2api.SafeError(e), "outcome": outcome,
				"uncertain": sub2api.IsMutationOutcomeUnknown(e), "result": result,
				"selected": selectedCount, "uploaded": uploadedCount, "skipped": selectedCount - uploadedCount,
			})
			return
		}
		if err := s.finishManualRepairTransfers(repairUploads, v); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "outcome": "partial", "error": "远端同步已完成，但本地待回传状态未能保存；请刷新核实，无需重新授权", "result": sub2api.PublicUploadResult(v)})
			return
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
		if err := s.jobs.CheckTOTPExport(j.ID); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
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
	for _, j := range selected {
		if err := s.jobs.CheckTOTPExport(j.ID); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
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
	if err := s.jobs.CheckTOTPExport(id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
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
	b, _, err := s.readJobExportData(id)
	if err != nil {
		w.Header().Del("Content-Disposition")
		status := http.StatusConflict
		if errors.Is(err, errJobExportMissing) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if job == nil || !job.ResultSaved {
		w.Header().Del("Content-Disposition")
		writeError(w, http.StatusConflict, "当前授权已失效或尚未保存，请重新登录授权后再下载")
		return
	}
	if err := s.jobs.CheckTOTPExport(id); err != nil {
		w.Header().Del("Content-Disposition")
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	current, ok := s.jobs.Get(id)
	if !ok || !current.ResultSaved {
		w.Header().Del("Content-Disposition")
		writeError(w, http.StatusConflict, "当前授权已失效或尚未保存，请重新登录授权后再下载")
		return
	}
	w.WriteHeader(200)
	_, _ = w.Write(b)
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
	exported := []string{}
	for _, job := range selected {
		if err := s.jobs.CheckTOTPExport(job.ID); err != nil {
			writeError(w, http.StatusConflict, err.Error())
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
		proxies = append(proxies, doc.Proxies...)
		exported = append(exported, job.ID)
	}
	if len(accounts) == 0 {
		writeError(w, http.StatusConflict, "选中的任务里没有已完成的导入文件")
		return
	}
	for _, job := range selected {
		if err := s.jobs.CheckTOTPExport(job.ID); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	for _, id := range exported {
		current, ok := s.jobs.Get(id)
		if !ok || !current.ResultSaved {
			writeError(w, http.StatusConflict, "选中账号的授权已失效，请刷新后重新选择有效授权")
			return
		}
	}
	w.Header().Set("Content-Disposition", `attachment; filename="sub2api-import-oauth-`+strconv.Itoa(len(accounts))+`-accounts-`+time.Now().UTC().Format("20060102-150405")+`.json"`)
	writeJSON(w, 200, map[string]any{"type": "sub2api-data", "version": 1, "exported_at": time.Now().UTC().Format(time.RFC3339Nano), "proxies": uniqueJSON(proxies), "accounts": accounts})
}

type jobExport struct {
	Type     string `json:"type"`
	Accounts []any  `json:"accounts"`
	Proxies  []any  `json:"proxies"`
}

var errJobExportMissing = errors.New("选中任务的导入文件不存在或无法读取")

func (s *Server) readJobExport(id string) (jobExport, error) {
	_, doc, err := s.readJobExportData(id)
	return doc, err
}

// Validate the selected account's export without rewriting its original bytes.
// Batch/upload decoding retains vendor numbers exactly through json.Number.
func (s *Server) readJobExportData(id string) ([]byte, jobExport, error) {
	job, ok := s.jobs.Get(id)
	if !ok {
		return nil, jobExport{}, errors.New("选中任务已不存在，请刷新后重试")
	}
	for _, name := range []string{"sub2api-import-oauth.json", "sub2api-data.json"} {
		data, err := os.ReadFile(filepath.Join(s.cfg.OutputRoot, id, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, jobExport{}, errors.New("选中任务的导入文件不存在或无法读取")
		}
		if authmaterial.ContainsLoginSessionJSON(data) {
			return nil, jobExport{}, errors.New("导入文件含已移除的登录会话材料，已停止导出或上传")
		}
		var doc jobExport
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if decoder.Decode(&doc) != nil || decoder.Decode(new(any)) != io.EOF || doc.Type != "sub2api-data" || len(doc.Accounts) != 1 {
			return nil, jobExport{}, errors.New("选中任务的导入文件格式不正确")
		}
		account, valid := doc.Accounts[0].(map[string]any)
		if !valid || sub2api.AccountEmail(account) != strings.ToLower(strings.TrimSpace(job.Email)) {
			return nil, jobExport{}, errors.New("导入文件与选中账号身份不一致，已停止导出或上传")
		}
		return data, doc, nil
	}
	return nil, jobExport{}, errJobExportMissing
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
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		writeError(w, 400, "invalid JSON")
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		writeError(w, 400, "request must contain one JSON value")
		return false
	}
	// Reject duplicate fields before Go's typed/map decoders can overwrite a
	// removed authentication option or change an operation's intended meaning.
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || authmaterial.ValidateUnambiguousJSON(raw) != nil || rejectNestedRequestFieldAliases(raw, reflect.TypeOf(v)) != nil {
		writeError(w, 400, "invalid JSON")
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		message := "invalid JSON"
		if errors.Is(err, errRemovedAuthenticationInputs) || errors.Is(err, errRemovedRemoteAuthentication) || errors.Is(err, errRemovedAuthenticationOperation) {
			message = err.Error()
		}
		writeError(w, 400, message)
		return false
	}
	return true
}

func writeMonitorConfigurationError(w http.ResponseWriter, err error) {
	if errors.Is(err, privatefile.ErrDurabilityUncertain) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "outcome": "unknown", "uncertain": true, "error": "监控配置已替换，但持久化未确认；请重新读取核对后明确保存，勿直接重复提交"})
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
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
