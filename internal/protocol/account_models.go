package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"tosub2-go-port/internal/transport"
)

// Upstream model descriptors require >= 0.153.0 for GPT-6.1 Sol and >= 0.155.0
// for GPT-6 Sol/Luna. Negotiate a manifest compatible with these generations.
const accountCodexModelsVersion = "0.155.0"
const accountCodexModelsURL = "https://chatgpt.com/backend-api/codex/models?client_version=" + accountCodexModelsVersion

type AccountModel struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"displayName"`
	ReasoningEfforts []string `json:"reasoningEfforts"`
}

type AccountModelCatalog struct {
	Models          []AccountModel `json:"models"`
	SuggestedModels []AccountModel `json:"suggestedModels,omitempty"`
	Source          string         `json:"source"`
	FetchedAt       time.Time      `json:"fetchedAt"`
	Message         string         `json:"message,omitempty"`
}

// AccountModelsError contains stable categories only. Upstream diagnostics,
// credentials and request URLs never become application-visible errors.
type AccountModelsError struct {
	Code       string `json:"code"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
}

func (e *AccountModelsError) Error() string {
	if e == nil {
		return "读取模型目录失败"
	}
	switch e.Code {
	case "not_found":
		return "账号不存在或已移入回收站"
	case "busy":
		return "账号正忙，请在当前操作结束后读取模型目录"
	case "ownership_unavailable":
		return "本机账号互斥保护不可用，未读取模型目录"
	case "superseded":
		return "账号授权或代理已改变，请重新读取模型目录"
	case "authorization_unavailable":
		return "没有可用的已保存授权，请先完成账号授权"
	case "recovery_pending":
		return "账号安全操作或授权恢复尚未完成，未读取模型目录"
	case "credentials_unavailable":
		return "安全存储暂不可读取，无法确认账号状态，未读取模型目录"
	case "authorization_expired":
		return "已保存授权已过期，请先更新授权"
	case "invalid_credentials", "identity_mismatch":
		return "模型目录的授权身份校验失败"
	case "proxy_selection_required":
		return "账号代理尚未明确，暂不可读取模型目录"
	case "transport_unavailable", "unsafe_transport":
		return "安全网络通道暂不可用，未读取模型目录"
	case "unauthorized":
		return "上游拒绝了该账号授权，未取得模型目录"
	case "forbidden":
		return "该账号无权访问此模型目录"
	case "rate_limited":
		return "模型目录查询受到上游限流，请稍后再试"
	case "redirect_blocked":
		return "模型目录请求发生重定向，已停止读取"
	case "timeout":
		return "读取模型目录超时"
	case "canceled":
		return "模型目录读取已取消"
	case "response_too_large":
		return "模型目录超过大小限制，未采用响应"
	case "invalid_response":
		return "上游模型目录格式无效，未采用本地预设替代"
	case "network_error":
		return "模型目录网络请求失败，请稍后重试"
	default:
		return "上游模型目录读取失败"
	}
}

func modelCatalogFailure(code string, status int) error {
	return &AccountModelsError{Code: code, HTTPStatus: status}
}

// Models reads one official information endpoint with the selected immutable
// grant. It never invokes inference, refreshes authorization or falls back to a
// local upload mapping. Every call reports a fresh account-scoped observation.
func (c *InsightsClient) Models(ctx context.Context, creds InsightCredentials) (AccountModelCatalog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method, err := NormalizeAuthorizationMethod(creds.AuthorizationMethod)
	if err != nil || !modelsHeaderValue(creds.AccessToken) || !modelsHeaderValue(creds.AccountID) {
		return AccountModelCatalog{}, modelCatalogFailure("invalid_credentials", 0)
	}
	if c == nil || c.Transport == nil {
		return AccountModelCatalog{}, modelCatalogFailure("transport_unavailable", 0)
	}
	if _, unsafe := c.Transport.(*Client); unsafe {
		return AccountModelCatalog{}, modelCatalogFailure("unsafe_transport", 0)
	}
	endpoint, source, budget := accountCodexModelsURL, "codex-models", 15*time.Second
	headers := http.Header{"Authorization": {"Bearer " + creds.AccessToken}, "Chatgpt-Account-Id": {creds.AccountID}, "Accept": {"application/json"}}
	if method == AuthorizationMethodBPS {
		endpoint, source, budget = insightBPSAccessURL, "bps-access", 5*time.Second
		headers = insightHeaders(endpoint, creds, "America/Los_Angeles")
	} else {
		// Keep client_version, Version and the paired client identity aligned,
		// as required by the upstream Codex model-manifest endpoint.
		headers.Set("Version", accountCodexModelsVersion)
		headers.Set("Originator", "codex_cli_rs")
		headers.Set("User-Agent", "codex_cli_rs/"+accountCodexModelsVersion+" (Ubuntu 22.4.0; x86_64) xterm-256color")
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if ctx.Err() != nil {
		return AccountModelCatalog{}, modelsContextFailure(ctx.Err())
	}
	deadline, _ := ctx.Deadline()
	response, err := c.Transport.Do(ctx, transport.Request{Method: http.MethodGet, URL: endpoint, Headers: headers, SingleAttempt: true, Timeout: time.Until(deadline), MaxResponseBytes: insightMaxBodyBytes})
	if ctx.Err() != nil {
		return AccountModelCatalog{}, modelsContextFailure(ctx.Err())
	}
	status := 0
	if response != nil {
		status = response.StatusCode
		if response.URL != nil && response.URL.String() != endpoint {
			return AccountModelCatalog{}, modelCatalogFailure("redirect_blocked", status)
		}
		if len(response.Body) > insightMaxBodyBytes {
			return AccountModelCatalog{}, modelCatalogFailure("response_too_large", status)
		}
	}
	if errors.Is(err, transport.ErrResponseTooLarge) {
		return AccountModelCatalog{}, modelCatalogFailure("response_too_large", status)
	}
	if response != nil && status != http.StatusOK {
		code := "upstream_error"
		switch {
		case status == http.StatusUnauthorized:
			code = "unauthorized"
		case status == http.StatusForbidden:
			code = "forbidden"
		case status == http.StatusTooManyRequests:
			code = "rate_limited"
		case status >= 300 && status < 400:
			code = "redirect_blocked"
		}
		return AccountModelCatalog{}, modelCatalogFailure(code, status)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return AccountModelCatalog{}, modelsContextFailure(err)
	}
	if err != nil || response == nil {
		return AccountModelCatalog{}, modelCatalogFailure("network_error", status)
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/html") || bytes.HasPrefix(bytes.TrimSpace(response.Body), []byte("<")) {
		return AccountModelCatalog{}, modelCatalogFailure("invalid_response", status)
	}
	var models, suggestions []AccountModel
	if method == AuthorizationMethodBPS {
		models, err = parseBPSAccountModels(response.Body, creds)
	} else {
		models, suggestions, err = parseCodexAccountModelChoices(response.Body, creds)
	}
	if err != nil {
		if errors.Is(err, errInsightIdentity) {
			return AccountModelCatalog{}, modelCatalogFailure("identity_mismatch", status)
		}
		var catalogErr *AccountModelsError
		if errors.As(err, &catalogErr) {
			// A business-level denial in an HTTP 200 body is not an HTTP 403.
			return AccountModelCatalog{}, modelCatalogFailure(catalogErr.Code, status)
		}
		return AccountModelCatalog{}, modelCatalogFailure("invalid_response", status)
	}
	if ctx.Err() != nil {
		return AccountModelCatalog{}, modelsContextFailure(ctx.Err())
	}
	catalog := AccountModelCatalog{Models: models, SuggestedModels: suggestions, Source: source, FetchedAt: time.Now().UTC()}
	if len(models) == 0 {
		catalog.Message = "该账号当前未提供模型目录"
	}
	return catalog, nil
}

func modelsContextFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return modelCatalogFailure("timeout", 0)
	}
	return modelCatalogFailure("canceled", 0)
}

func modelsHeaderValue(value string) bool {
	if value == "" || len(value) > 32768 {
		return false
	}
	for _, r := range value {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func parseBPSAccountModels(body []byte, creds InsightCredentials) ([]AccountModel, error) {
	access, err := parseInsightBPS(body, creds, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if access.Allowed == nil {
		return nil, errInsightProtocol
	}
	if !*access.Allowed {
		return nil, modelCatalogFailure("forbidden", http.StatusForbidden)
	}
	models := []AccountModel{}
	for _, entry := range access.Models {
		if entry.Restricted || entry.Available != nil && !*entry.Available {
			continue
		}
		label := strings.TrimSpace(entry.Label)
		if label == "" {
			label = entry.ID
		}
		models = append(models, AccountModel{ID: entry.ID, DisplayName: label, ReasoningEfforts: append([]string{}, entry.ReasoningEfforts...)})
	}
	return models, nil
}

func parseCodexAccountModels(body []byte, creds InsightCredentials) ([]AccountModel, error) {
	models, _, err := parseCodexAccountModelChoices(body, creds)
	return models, err
}

func parseCodexAccountModelChoices(body []byte, creds InsightCredentials) ([]AccountModel, []AccountModel, error) {
	envelope, err := insightObject(body)
	if err != nil {
		return nil, nil, err
	}
	if err := insightIdentity(envelope, creds); err != nil {
		return nil, nil, err
	}
	entries, err := insightField[[]json.RawMessage](envelope, "models")
	if err != nil || entries == nil || len(*entries) > 1024 {
		return nil, nil, errInsightProtocol
	}
	models := []AccountModel{}
	seen := map[string]bool{}
	for _, raw := range *entries {
		entry, err := insightObject(raw)
		if err != nil {
			return nil, nil, err
		}
		id, err := insightField[string](entry, "slug")
		if err != nil {
			return nil, nil, err
		}
		if id == nil || *id == "" {
			id, err = insightField[string](entry, "id")
			if err != nil {
				return nil, nil, err
			}
		}
		if id == nil || !accountModelID(*id) {
			continue
		}
		if seen[*id] {
			return nil, nil, errInsightProtocol
		}
		seen[*id] = true
		// Codex visibility and model_picker_enabled control the official client
		// picker, not whether this account can call a returned model. Keep those
		// entries, as sub2api does when converting this same manifest.
		disabled := false
		for _, key := range []string{"available", "enabled"} {
			v, err := insightField[bool](entry, key)
			if err != nil {
				return nil, nil, err
			}
			disabled = disabled || v != nil && !*v
		}
		if disabled {
			continue
		}
		label, err := insightField[string](entry, "display_name")
		if err != nil {
			return nil, nil, err
		}
		displayName := *id
		if label != nil && strings.TrimSpace(*label) != "" {
			if !insightSafeText(*label, 160) {
				return nil, nil, errInsightProtocol
			}
			displayName = strings.TrimSpace(*label)
		}
		efforts := []string{}
		effortSeen := map[string]bool{}
		for _, key := range []string{"supported_reasoning_levels", "reasoning_efforts"} {
			levels, err := insightField[[]json.RawMessage](entry, key)
			if err != nil {
				return nil, nil, err
			}
			if levels == nil {
				continue
			}
			if len(*levels) > 16 {
				return nil, nil, errInsightProtocol
			}
			for _, rawLevel := range *levels {
				var effort string
				if len(rawLevel) > 0 && rawLevel[0] == '"' {
					if json.Unmarshal(rawLevel, &effort) != nil {
						return nil, nil, errInsightProtocol
					}
				} else {
					level, err := insightObject(rawLevel)
					if err != nil {
						return nil, nil, err
					}
					value, err := insightField[string](level, "effort")
					if err != nil || value == nil {
						return nil, nil, errInsightProtocol
					}
					effort = *value
				}
				switch effort {
				case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
					if !effortSeen[effort] {
						efforts = append(efforts, effort)
						effortSeen[effort] = true
					}
				}
			}
		}
		models = append(models, AccountModel{ID: *id, DisplayName: displayName, ReasoningEfforts: efforts})
	}
	return models, codexCommonModelSuggestions(seen), nil
}

func accountModelID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}
