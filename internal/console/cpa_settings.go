package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"tosub2-go-port/internal/cpa"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/store"
)

// Only the opaque credential reference is written with the public settings.
// A new key uses a new reference, so a failed settings commit cannot replace
// the key still referenced by the previous configuration.
type cpaStoredSettings struct {
	Version       int    `json:"version"`
	Revision      int64  `json:"revision"`
	BaseURL       string `json:"baseUrl"`
	CredentialRef string `json:"credentialRef,omitempty"`
	cpa.UploadOptions
	cpaMonitorOptions
}

type cpaSchedulingUpdate struct {
	Priority          *int
	Weight            *int
	MonitorEnabled    *bool
	ExcludedPlanTypes *[]string
	IntervalMinutes   *int
	CooldownMinutes   *int
}

type cpaSettingsState struct {
	cpaStoredSettings
	key        string
	persistent bool
	loadError  string
}

var cpaCredentialReference = regexp.MustCompile(`^cpa-management-[a-f0-9]{32}$`)

func (s *Server) initializeCPA() {
	s.cpa = cpaSettingsState{cpaStoredSettings: cpaStoredSettings{Version: 1, cpaMonitorOptions: defaultCPAMonitorOptions()}}
	s.cpaMonitor.wake = make(chan struct{}, 1)
	defer s.updateCPAAutoRepairPolicyLocked()
	if s.cfg.OutputRoot == "" {
		return
	}
	saved := cpaStoredSettings{cpaMonitorOptions: defaultCPAMonitorOptions()}
	err := (&store.Store{Root: s.cfg.OutputRoot}).ReadJSON(context.Background(), "cpa-settings.json", &saved)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || saved.Version != 1 || saved.Revision < 0 || saved.CredentialRef != "" && !cpaCredentialReference.MatchString(saved.CredentialRef) {
		s.cpa.loadError = "CPA 配置文件无法读取，请核对后重新保存配置"
		return
	}
	if err := saved.UploadOptions.Validate(); err != nil {
		s.cpa.loadError = "CPA 调度配置无效，请核对后重新保存配置"
		return
	}
	if err := saved.cpaMonitorOptions.normalize(); err != nil {
		s.cpa.loadError = "CPA 自动修复配置无效，请核对后重新保存配置"
		return
	}
	if saved.BaseURL != "" {
		base, err := cpa.ValidateBaseURL(saved.BaseURL)
		if err != nil || base != saved.BaseURL {
			s.cpa.loadError = "CPA 配置地址无效，请重新配置"
			return
		}
	}
	s.cpa.cpaStoredSettings = saved
	if saved.CredentialRef != "" && s.monitorVault != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		secret, err := credentials.LoadContext(ctx, s.monitorVault, saved.CredentialRef)
		if err == nil {
			s.cpa.key, s.cpa.persistent = secret.Password, true
		} else {
			s.cpa.loadError = "CPA 管理密钥无法读取，请重新填写并保存"
		}
	}
}

func (s *Server) publicCPASettings() map[string]any {
	s.cpaMu.Lock()
	defer s.cpaMu.Unlock()
	persistence := "memory_only"
	if s.cpa.persistent {
		persistence = "secure"
	}
	message := s.cpa.loadError
	if s.cpaMonitor.uncertain {
		message = "CPA 配置持久化未确认，自动修复已暂停；请重新保存配置"
	}
	return map[string]any{"baseUrl": s.cpa.BaseURL, "revision": s.cpa.Revision,
		"priority": s.cpa.Priority, "weight": s.cpa.EffectiveWeight(),
		"monitorEnabled": s.cpa.MonitorEnabled, "excludedPlanTypes": append([]string{}, s.cpa.ExcludedPlanTypes...),
		"intervalMinutes": s.cpa.IntervalMinutes, "cooldownMinutes": s.cpa.CooldownMinutes,
		"configured": s.cpaConfiguredLocked(),
		"hasApiKey":  s.cpa.key != "", "credentialPersistence": persistence, "error": message}
}

func (s *Server) configuredCPA() (cpa.Config, error) {
	s.cpaMu.Lock()
	defer s.cpaMu.Unlock()
	if s.cpa.loadError != "" || s.cpa.BaseURL == "" || s.cpa.key == "" {
		return cpa.Config{}, errors.New("请先在服务配置中保存 CPA 管理地址和管理密钥")
	}
	return cpa.Config{BaseURL: s.cpa.BaseURL, APIKey: s.cpa.key, UploadOptions: s.cpa.UploadOptions.Clone()}, nil
}

func (s *Server) cpaAPI(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/cpa/monitor", "/api/cpa/monitor/check":
		s.cpaMonitorAPI(w, r)
	case "/api/cpa/settings":
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, s.publicCPASettings())
		case http.MethodPost:
			s.saveCPASettings(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "/api/cpa/upload":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.uploadCPA(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown CPA route")
	}
}

func (s *Server) saveCPASettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL           *string   `json:"baseUrl"`
		AdminAPIKey       *string   `json:"adminApiKey"`
		Revision          *int64    `json:"revision"`
		Priority          *int      `json:"priority"`
		Weight            *int      `json:"weight"`
		MonitorEnabled    *bool     `json:"monitorEnabled"`
		ExcludedPlanTypes *[]string `json:"excludedPlanTypes"`
		IntervalMinutes   *int      `json:"intervalMinutes"`
		CooldownMinutes   *int      `json:"cooldownMinutes"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.BaseURL == nil || req.Revision == nil || *req.Revision < 0 {
		writeError(w, http.StatusBadRequest, "CPA 配置需要地址和当前版本")
		return
	}
	if s.poolMonitorGate.Load() != nil {
		s.poolMonitorMu.Lock()
		defer s.poolMonitorMu.Unlock()
		if s.poolMonitorChecking {
			writeError(w, http.StatusConflict, "巡检正在进行，请等待结束后保存服务配置")
			return
		}
		s.cpaMu.Lock()
		running := s.cpaMonitor.running
		s.cpaMu.Unlock()
		if running {
			writeError(w, http.StatusConflict, "CPA 巡检正在进行，请等待结束后保存服务配置")
			return
		}
		if req.MonitorEnabled != nil || req.ExcludedPlanTypes != nil || req.IntervalMinutes != nil || req.CooldownMinutes != nil {
			writeError(w, http.StatusConflict, "巡检规则已统一，请在号池监控中选择目标并保存")
			return
		}
		// A newly configured connection adopts the shared switch, without
		// changing its target or any rules.
		enabled := s.poolMonitorAllows("cpa")
		req.MonitorEnabled = &enabled
	}
	base := strings.TrimSpace(*req.BaseURL)
	if base != "" {
		var err error
		base, err = cpa.ValidateBaseURL(base)
		if err != nil {
			writeError(w, http.StatusBadRequest, cpa.SafeError(err))
			return
		}
	}
	key := ""
	if req.AdminAPIKey != nil {
		key = strings.TrimSpace(*req.AdminAPIKey)
	}
	status, err := s.commitCPASettings(r.Context(), base, key, *req.Revision, cpaSchedulingUpdate{Priority: req.Priority, Weight: req.Weight,
		MonitorEnabled: req.MonitorEnabled, ExcludedPlanTypes: req.ExcludedPlanTypes, IntervalMinutes: req.IntervalMinutes, CooldownMinutes: req.CooldownMinutes})
	if err != nil {
		writeCPASettingsError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, s.publicCPASettings())
}

func writeCPASettingsError(w http.ResponseWriter, status int, err error) {
	if errors.Is(err, store.ErrDurabilityUncertain) || errors.Is(err, privatefile.ErrDurabilityUncertain) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "CPA 配置已替换，但持久化未确认；请核对保存结果后再操作", "outcome": "unknown", "uncertain": true})
	} else {
		writeJSON(w, status, map[string]any{"error": err.Error(), "outcome": "failed", "uncertain": false})
	}
}

func (s *Server) commitCPASettings(ctx context.Context, base, key string, revision int64, updates ...cpaSchedulingUpdate) (int, error) {
	s.cpaMu.Lock()
	defer s.cpaMu.Unlock()
	// A wrapped revision would persist a negative version and make restart
	// discard the configured destination and its protected credential reference.
	if revision != s.cpa.Revision || revision == math.MaxInt64 {
		return http.StatusConflict, errors.New("CPA 配置已更新，请重新读取后保存")
	}
	options := s.cpa.UploadOptions.Clone()
	monitor := s.cpa.cpaMonitorOptions.clone()
	for _, update := range updates {
		if update.Priority != nil {
			options.Priority = *update.Priority
		}
		if update.Weight != nil {
			weight := *update.Weight
			options.Weight = &weight
		}
		if update.MonitorEnabled != nil {
			monitor.MonitorEnabled = *update.MonitorEnabled
		}
		if update.ExcludedPlanTypes != nil {
			monitor.ExcludedPlanTypes = append([]string{}, (*update.ExcludedPlanTypes)...)
		}
		if update.IntervalMinutes != nil {
			monitor.IntervalMinutes = *update.IntervalMinutes
		}
		if update.CooldownMinutes != nil {
			monitor.CooldownMinutes = *update.CooldownMinutes
		}
	}
	if err := options.Validate(); err != nil {
		return http.StatusBadRequest, err
	}
	if err := monitor.normalize(); err != nil {
		return http.StatusBadRequest, err
	}
	if base == "" {
		monitor.MonitorEnabled = false
	}
	next := cpaSettingsState{cpaStoredSettings: cpaStoredSettings{Version: 1, Revision: revision + 1, BaseURL: base, UploadOptions: options, cpaMonitorOptions: monitor}}
	if base != "" {
		if key == "" && base == s.cpa.BaseURL {
			next.key, next.CredentialRef, next.persistent = s.cpa.key, s.cpa.CredentialRef, s.cpa.persistent
		} else if key != "" {
			if _, err := cpa.ParseUploadSettings(map[string]any{"baseUrl": base, "adminApiKey": key}); err != nil {
				return http.StatusBadRequest, errors.New("CPA 管理密钥格式无效")
			}
			next.key = key
			if s.monitorVault != nil {
				var id [16]byte
				if _, err := rand.Read(id[:]); err != nil {
					return http.StatusInternalServerError, errors.New("无法准备 CPA 凭据存储")
				}
				next.CredentialRef = "cpa-management-" + hex.EncodeToString(id[:])
				credentialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				err := credentials.SaveContext(credentialCtx, s.monitorVault, next.CredentialRef, credentials.Credentials{Password: key})
				cancel()
				if err != nil {
					return http.StatusInternalServerError, errors.New("CPA 管理密钥未能保存到安全凭据服务，原配置保留")
				}
				next.persistent = true
			}
		}
		if next.key == "" {
			return http.StatusBadRequest, errors.New("请填写此 CPA 地址对应的管理密钥")
		}
	}
	var err error
	if s.cfg.OutputRoot == "" {
		err = errors.New("missing output root")
	} else {
		err = (&store.Store{Root: s.cfg.OutputRoot}).WriteJSON(ctx, "cpa-settings.json", next.cpaStoredSettings)
	}
	uncertain := errors.Is(err, store.ErrDurabilityUncertain) || errors.Is(err, privatefile.ErrDurabilityUncertain)
	if err != nil && !uncertain {
		if next.CredentialRef != "" && next.CredentialRef != s.cpa.CredentialRef {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = credentials.DeleteContext(cleanupCtx, s.monitorVault, next.CredentialRef)
			cancel()
		}
		return http.StatusInternalServerError, errors.New("CPA 配置保存失败，原配置保留")
	}
	oldRef := s.cpa.CredentialRef
	s.cpa = next
	s.cpaMonitor.uncertain = uncertain
	s.cpaMonitor.nextCheckAt = nil
	s.updateCPAAutoRepairPolicyLocked()
	select {
	case s.cpaMonitor.wake <- struct{}{}:
	default:
	}
	// Keep both key generations if directory durability is unknown: either
	// metadata generation may survive a crash. Never delete an active key.
	if !uncertain && oldRef != "" && oldRef != next.CredentialRef && s.monitorVault != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = credentials.DeleteContext(cleanupCtx, s.monitorVault, oldRef)
		cancel()
	}
	return http.StatusOK, err
}

// Freeze the saved destination/key into the explicit operation's private input.
// Later settings edits must not silently redirect an already queued upload.
func (s *Server) prepareUploadInputs(values map[string]any) error {
	target, err := jobs.UploadTargetForValues(values)
	if err != nil {
		return err
	}
	uploadRelogin, _ := values["uploadAfterRelogin"].(bool)
	uploadTOTP, _ := values["uploadAfterTotp"].(bool)
	if target != "cpa" || !uploadRelogin && !uploadTOTP {
		return nil
	}
	if _, explicit := values["cpaConfig"]; explicit {
		_, err := cpa.ParseUploadSettings(values["cpaConfig"])
		return err
	}
	cfg, err := s.configuredCPA()
	if err != nil {
		return err
	}
	values["cpaConfig"] = map[string]any{"baseUrl": cfg.BaseURL, "adminApiKey": cfg.APIKey, "priority": cfg.Priority, "weight": cfg.EffectiveWeight()}
	return nil
}

func validateCPAOperationAccount(job *jobs.Job, values map[string]any) error {
	target, err := jobs.UploadTargetForValues(values)
	if err != nil {
		return err
	}
	uploadRelogin, _ := values["uploadAfterRelogin"].(bool)
	uploadTOTP, _ := values["uploadAfterTotp"].(bool)
	if target != jobs.UploadTargetCPA || !uploadRelogin && !uploadTOTP {
		return nil
	}
	method, err := jobs.AuthorizationMethodForValues(job.AuthorizationMethod, values)
	if err != nil {
		return err
	}
	if method == jobs.AuthorizationMethodBPS {
		return cpa.ErrUnsupportedAuthorization
	}
	return nil
}
