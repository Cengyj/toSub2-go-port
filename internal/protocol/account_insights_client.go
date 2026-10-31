package protocol

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tosub2-go-port/internal/transport"
)

const (
	insightUsageURL         = "https://chatgpt.com/backend-api/wham/usage"
	insightResetCreditsURL  = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	insightBPSAccessURL     = "https://bps.openai.com/basispoints/api/responses/access?include_models=true"
	insightAccountsURL      = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27"
	insightSubscriptionsURL = "https://chatgpt.com/backend-api/subscriptions"
	insightMaxBodyBytes     = 1 << 20
)

var errInsightProtocol = errors.New("invalid account insight response")
var errInsightIdentity = errors.New("account insight identity mismatch")

// Query only reads information endpoints. Errors are source-specific; one
// rejection must not erase another source's successful, independently dated data.
func (c *InsightsClient) Query(ctx context.Context, creds InsightCredentials, options InsightsOptions) InsightResult {
	result := InsightResult{Errors: []InsightError{}, AttemptedComponents: []string{}}
	attempted := func(component string) {
		if !slices.Contains(result.AttemptedComponents, component) {
			result.AttemptedComponents = append(result.AttemptedComponents, component)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	method, err := NormalizeAuthorizationMethod(creds.AuthorizationMethod)
	creds.AuthorizationMethod = method
	creds.AccountID = strings.TrimSpace(creds.AccountID)
	creds.Email = strings.TrimSpace(creds.Email)
	if c == nil || c.Transport == nil {
		result.Errors = append(result.Errors, insightFailure("query", "transport_unavailable"))
		return result
	}
	if _, authenticationClient := c.Transport.(*Client); authenticationClient {
		result.Errors = append(result.Errors, insightFailure("query", "unsafe_transport"))
		return result
	}
	if err != nil || strings.TrimSpace(creds.AccessToken) == "" || strings.TrimSpace(creds.AccountID) == "" || strings.ContainsAny(creds.AccessToken+creds.AccountID, "\r\n") {
		result.Errors = append(result.Errors, insightFailure("query", "invalid_credentials"))
		return result
	}
	if options.Timezone == "" {
		options.Timezone = "America/Los_Angeles"
	}
	if _, err := time.LoadLocation(options.Timezone); err != nil {
		result.Errors = append(result.Errors, insightFailure("query", "invalid_timezone"))
		return result
	}
	if !slices.Contains(options.SkipComponents, "usage") {
		if body, failure := c.getWithAttempt(ctx, "usage", insightUsageURL, creds, options, 15*time.Second, attempted); failure != nil {
			result.Errors = append(result.Errors, *failure)
		} else if usage, err := parseInsightUsage(body, creds, time.Now().UTC()); err != nil {
			result.Errors = append(result.Errors, insightParseFailure("usage", err))
		} else {
			result.Usage = usage
		}
	}
	if method == AuthorizationMethodBPS && !slices.Contains(options.SkipComponents, "bps") {
		if body, failure := c.getWithAttempt(ctx, "bps", insightBPSAccessURL, creds, options, 5*time.Second, attempted); failure != nil {
			result.Errors = append(result.Errors, *failure)
		} else if access, err := parseInsightBPS(body, creds, time.Now().UTC()); err != nil {
			result.Errors = append(result.Errors, insightParseFailure("bps", err))
		} else {
			result.BPS = access
		}
	}
	if !slices.Contains(options.SkipComponents, "subscription") {
		result.Subscription, result.Errors = c.querySubscription(ctx, creds, options, result.Errors, attempted)
	}
	if options.IncludeResetCredits && result.Usage != nil && !slices.Contains(options.SkipComponents, "resetCredits") {
		if body, failure := c.getWithAttempt(ctx, "resetCredits", insightResetCreditsURL, creds, options, 5*time.Second, attempted); failure != nil {
			result.Errors = append(result.Errors, *failure)
		} else if details, err := parseInsightResetDetails(body, time.Now().UTC()); err != nil {
			result.Errors = append(result.Errors, insightParseFailure("resetCredits", err))
		} else {
			result.Usage.ResetCredits = details
		}
	}
	return result
}

func insightAllowedURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.RawPath != "" || u.Port() != "" {
		return false
	}
	if rawURL == insightUsageURL || rawURL == insightResetCreditsURL || rawURL == insightBPSAccessURL || rawURL == insightAccountsURL {
		return true
	}
	if u.Host != "chatgpt.com" || u.Path != "/backend-api/subscriptions" {
		return false
	}
	q, err := url.ParseQuery(u.RawQuery)
	return err == nil && len(q) == 1 && len(q["account_id"]) == 1 && strings.TrimSpace(q.Get("account_id")) != ""
}

func insightHeaders(rawURL string, creds InsightCredentials, timezone string) http.Header {
	h := http.Header{"Authorization": {"Bearer " + creds.AccessToken}, "Accept": {"application/json"}, "Chatgpt-Account-Id": {creds.AccountID}, "Oai-Timezone": {timezone}}
	if rawURL == insightBPSAccessURL {
		h.Set("X-Openai-Account-Id", creds.AccountID)
		h.Set("X-Basispoints-Auth-Mode", "chatgpt")
		h.Set("Origin", "https://bps.openai.com")
		h.Set("User-Agent", "Mozilla/5.0")
		h.Set("X-Openai-Internal-Basispoints-Client-Product", "basispoints-excel-plugin")
		h.Set("X-Openai-Internal-Basispoints-Client-Agent-Profile", "excel")
	} else if rawURL == insightUsageURL || rawURL == insightResetCreditsURL {
		h.Set("Openai-Beta", "codex-1")
		h.Set("Originator", "Codex Desktop")
		h.Set("Oai-Language", "zh-CN")
		h.Set("Sec-Fetch-Site", "none")
		h.Set("Sec-Fetch-Mode", "no-cors")
		h.Set("Sec-Fetch-Dest", "empty")
	} else {
		h.Set("Origin", "https://chatgpt.com")
		h.Set("Referer", "https://chatgpt.com/")
	}
	return h
}

func (c *InsightsClient) get(ctx context.Context, component, rawURL string, creds InsightCredentials, options InsightsOptions, budget time.Duration) ([]byte, *InsightError) {
	return c.getWithAttempt(ctx, component, rawURL, creds, options, budget, nil)
}

func (c *InsightsClient) getWithAttempt(ctx context.Context, component, rawURL string, creds InsightCredentials, options InsightsOptions, budget time.Duration, attempted func(string)) ([]byte, *InsightError) {
	if !insightAllowedURL(rawURL) {
		e := insightFailure(component, "endpoint_rejected")
		return nil, &e
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			e := insightContextFailure(component, ctx.Err())
			return nil, &e
		}
		if attempted != nil {
			attempted(component)
		}
		response, err := c.Transport.Do(ctx, transport.Request{Method: http.MethodGet, URL: rawURL, Headers: insightHeaders(rawURL, creds, options.Timezone), SingleAttempt: true, Timeout: budget, MaxResponseBytes: insightMaxBodyBytes})
		if response != nil {
			if response.URL != nil && response.URL.String() != rawURL {
				e := insightFailure(component, "redirect_blocked")
				return nil, &e
			}
			if len(response.Body) > insightMaxBodyBytes {
				e := insightFailure(component, "response_too_large")
				e.HTTPStatus = response.StatusCode
				return nil, &e
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				e := insightContextFailure(component, ctx.Err())
				return nil, &e
			}
			if errors.Is(err, transport.ErrResponseTooLarge) {
				e := insightFailure(component, "response_too_large")
				if response != nil {
					e.HTTPStatus = response.StatusCode
				}
				return nil, &e
			}
			// Received headers remain authoritative even if the error body is
			// truncated. Preserve a rejection's cooldown and never retry it.
			if response != nil && response.StatusCode >= 300 {
				e := insightHTTPFailure(component, response)
				e.TransportReason = insightTransportReason(err)
				return nil, &e
			}
			if attempt == 0 && transport.IsRetryableReadError(err) {
				timer := time.NewTimer(time.Duration(75+rand.IntN(75)) * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
				case <-timer.C:
				}
				continue
			}
			e := insightNetworkFailure(component, err)
			return nil, &e
		}
		if response == nil {
			e := insightNetworkFailure(component, nil)
			return nil, &e
		}
		if ctx.Err() != nil {
			e := insightContextFailure(component, ctx.Err())
			return nil, &e
		}
		if response.StatusCode != http.StatusOK {
			e := insightHTTPFailure(component, response)
			return nil, &e
		}
		if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/html") || bytes.HasPrefix(bytes.TrimSpace(response.Body), []byte("<")) {
			e := insightFailure(component, "challenge_or_html")
			return nil, &e
		}
		return response.Body, nil
	}
	e := insightNetworkFailure(component, nil)
	return nil, &e
}

func insightHTTPFailure(component string, response *transport.Response) InsightError {
	code := "upstream_error"
	switch response.StatusCode {
	case 401:
		code = "authorization_rejected"
	case 403:
		code = "permission_denied"
	case 429:
		code = "rate_limited"
	default:
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			code = "redirect_blocked"
		}
	}
	upstreamCode, upstreamType := insightUpstreamDiagnostics(response.Body)
	if (response.StatusCode == 400 || response.StatusCode == 403) && (upstreamCode == "insufficient_scope" || upstreamCode == "unsupported_token_type") {
		code = "unsupported_authorization"
	}
	e := insightFailure(component, code)
	e.HTTPStatus = response.StatusCode
	e.UpstreamCode, e.UpstreamType = upstreamCode, upstreamType
	if response.StatusCode == http.StatusPaymentRequired {
		// Status 402 alone cannot establish an expired plan or unpaid bill.
		e.Message = "查询接口返回 HTTP 402，未取得该来源数据"
	}
	if response.StatusCode == 429 || response.StatusCode == 503 {
		e.RetryAfter = insightRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
	}
	return e
}

// Error fields are attacker-controlled too. Retain only recognized protocol
// constants, never free-form message, request IDs, headers, URLs or body text.
// Duplicate keys, trailing JSON, HTML and oversized bodies are rejected by the
// same strict parser as observations before any diagnostic can be copied.
func insightUpstreamDiagnostics(body []byte) (code, kind string) {
	fields, err := insightObject(body)
	if err != nil {
		return "", ""
	}
	detail, err := insightObject(fields["error"])
	if err != nil {
		return "", ""
	}
	if value, err := insightField[string](detail, "code"); err == nil && value != nil {
		switch *value {
		case "insufficient_scope", "unsupported_token_type", "invalid_token", "token_expired", "token_revoked", "invalid_api_key", "account_deactivated", "account_suspended", "access_denied", "permission_denied", "rate_limit_exceeded", "insufficient_quota", "quota_exceeded", "billing_not_active", "billing_hard_limit_reached", "billing_limit_reached", "payment_required", "unsupported_country_region_territory", "invalid_request_error", "server_error", "internal_server_error", "service_unavailable", "temporarily_unavailable":
			code = *value
		}
	}
	if value, err := insightField[string](detail, "type"); err == nil && value != nil {
		switch *value {
		case "invalid_request_error", "authentication_error", "permission_error", "rate_limit_error", "insufficient_quota", "billing_error", "server_error", "api_error":
			kind = *value
		}
	}
	return code, kind
}

func insightNetworkFailure(component string, err error) InsightError {
	e := insightFailure(component, "network_error")
	e.TransportReason = insightTransportReason(err)
	e.Message += "（" + e.TransportReason + "）"
	return e
}

func insightTransportReason(err error) string {
	if err == nil {
		return "未收到 HTTP 响应"
	}
	// Production transports discard raw causes and expose only this closed
	// diagnostic vocabulary. Typed raw errors can also be returned by a Doer.
	var diagnostic *transport.TransportError
	if errors.As(err, &diagnostic) {
		return transport.ReadFailureMessage(err)
	}
	var dns *net.DNSError
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var roots x509.SystemRootsError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	switch {
	case errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &hostname), errors.As(err, &roots):
		return "证书验证失败"
	case errors.As(err, &record), errors.As(err, &alert):
		return "TLS 协商或协议不兼容"
	case errors.As(err, &dns):
		return "域名解析失败"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "连接被拒绝"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN), errors.Is(err, syscall.EHOSTDOWN):
		return "网络不可达"
	case errors.Is(err, context.Canceled):
		return "请求已取消"
	default:
		return transport.ReadFailureMessage(err)
	}
}

func insightRetryAfter(value string, now time.Time) *time.Time {
	value = strings.TrimSpace(value)
	var until time.Time
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= 86400 {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if parsed, err := http.ParseTime(value); err == nil {
		until = parsed.UTC()
	}
	if until.IsZero() || until.Before(now) {
		return nil
	}
	if until.After(now.Add(24 * time.Hour)) {
		until = now.Add(24 * time.Hour)
	}
	return &until
}

func insightContextFailure(component string, err error) InsightError {
	if errors.Is(err, context.DeadlineExceeded) {
		return insightFailure(component, "timeout")
	}
	return insightFailure(component, "canceled")
}

func insightParseFailure(component string, err error) InsightError {
	if errors.Is(err, errInsightIdentity) {
		return insightFailure(component, "identity_mismatch")
	}
	return insightFailure(component, "protocol_error")
}

func insightFailure(component, code string) InsightError {
	messages := map[string]string{
		"transport_unavailable": "查询传输不可用", "unsafe_transport": "查询不能使用认证挑战传输", "invalid_credentials": "当前授权缺少有效查询身份", "invalid_timezone": "请求时区无效", "endpoint_rejected": "查询端点不在只读白名单", "response_too_large": "查询响应超过安全上限", "redirect_blocked": "查询重定向已阻止", "network_error": "查询网络请求失败", "timeout": "查询超时", "canceled": "查询已取消", "challenge_or_html": "查询收到安全挑战或非JSON响应", "authorization_rejected": "当前授权被查询接口拒绝", "permission_denied": "当前授权的查询权限受限", "rate_limited": "查询被上游限流", "upstream_error": "上游查询暂不可用", "unsupported_authorization": "当前授权明确不支持此查询", "identity_mismatch": "查询结果的账号身份不匹配", "protocol_error": "查询响应格式不符合预期", "subscription_scope_mismatch": "订阅套餐或工作区归属待确认", "identity_unverified": "订阅结果尚未确认账号归属",
	}
	return InsightError{Component: component, Code: code, Message: messages[code]}
}

func insightObject(body []byte) (map[string]json.RawMessage, error) {
	if !insightValidJSON(body) {
		return nil, errInsightProtocol
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errInsightProtocol
	}
	return fields, nil
}

// Reject duplicate keys before decoding maps. Otherwise a conflicting identity
// may be silently replaced by a later JSON member, including in nested objects.
func insightValidJSON(body []byte) bool {
	if len(body) == 0 || len(body) > insightMaxBodyBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if insightJSONValue(decoder, 0) != nil {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func insightJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errInsightProtocol
	}
	token, err := decoder.Token()
	if err != nil {
		return errInsightProtocol
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	var closing json.Delim
	switch delimiter {
	case '{':
		closing = '}'
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return errInsightProtocol
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errInsightProtocol
			}
			seen[name] = true
			if err := insightJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		closing = ']'
		for decoder.More() {
			if err := insightJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errInsightProtocol
	}
	end, err := decoder.Token()
	if err != nil || end != closing {
		return errInsightProtocol
	}
	return nil
}

func insightField[T any](fields map[string]json.RawMessage, name string) (*T, error) {
	raw, exists := fields[name]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, errInsightProtocol
	}
	return &result, nil
}

func insightIdentity(fields map[string]json.RawMessage, creds InsightCredentials) error {
	for _, name := range []string{"account_id", "chatgpt_account_id", "accountId"} {
		value, err := insightField[string](fields, name)
		if err != nil {
			return err
		}
		if value != nil && strings.TrimSpace(*value) != "" && strings.TrimSpace(*value) != strings.TrimSpace(creds.AccountID) {
			return errInsightIdentity
		}
	}
	email, err := insightField[string](fields, "email")
	if err != nil {
		return err
	}
	if email != nil && *email != "" && creds.Email != "" && !strings.EqualFold(strings.TrimSpace(*email), strings.TrimSpace(creds.Email)) {
		return errInsightIdentity
	}
	return nil
}
