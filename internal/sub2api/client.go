// Package sub2api contains the authenticated Sub2API admin client and monitor.
package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tosub2-go-port/internal/transport"
)

type Config struct {
	CodexFingerprintMode string `json:"codexFingerprintMode,omitempty"`
	BaseURL              string `json:"baseUrl"`
	APIKey               string `json:"-"`
	// Timeout limits each attempt to at most 30 seconds. The retry budget is independent,
	// and an earlier caller deadline still stops the operation immediately.
	Timeout           time.Duration                  `json:"-"`
	GroupIDs          []int                          `json:"groupIds"`
	ExcludedPlanTypes []string                       `json:"excludedPlanTypes"`
	OnRetry           func(transport.ReadRetryEvent) `json:"-"`
}
type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.GroupIDs = append([]int(nil), cfg.GroupIDs...)
	cfg.ExcludedPlanTypes = NormalizeExcludedPlanTypes(cfg.ExcludedPlanTypes)
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// RemoteError contains status only: response bodies and URLs can contain tokens.
type RemoteError struct{ StatusCode int }

func (e *RemoteError) Error() string { return fmt.Sprintf("Sub2API returned HTTP %d", e.StatusCode) }
func IsRemoteStatus(err error, code int) bool {
	var e *RemoteError
	return errors.As(err, &e) && e.StatusCode == code
}
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "Sub2API operation canceled"
	}
	var network *transport.TransportError
	if errors.As(err, &network) && transport.IsRetryableReadError(err) {
		return "Sub2API " + transport.ReadFailureMessage(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Sub2API operation timed out"
	}
	return "Sub2API operation failed"
}

func normalizeBaseURL(raw string) (string, error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid Sub2API base URL")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

type requestOptions struct{ Headers http.Header }

func (c *Client) request(ctx context.Context, method, path string, body any, out any) error {
	return c.requestWithOptions(ctx, method, path, body, out, requestOptions{})
}
func (c *Client) requestWithOptions(ctx context.Context, method, path string, body any, out any, options requestOptions) error {
	if strings.TrimSpace(c.cfg.BaseURL) == "" || strings.TrimSpace(c.cfg.APIKey) == "" {
		return errors.New("Sub2API URL and API key are required")
	}
	base, err := normalizeBaseURL(c.cfg.BaseURL)
	if err != nil {
		return err
	}
	if len(c.cfg.APIKey) > 512 || strings.ContainsAny(c.cfg.APIKey, "\r\n") {
		return errors.New("invalid Sub2API API key")
	}
	u, err := url.Parse(base + "/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return errors.New("invalid Sub2API request path")
	}
	var requestBody []byte
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return errors.New("invalid Sub2API request body")
		}
		requestBody = b
	}
	if ctx == nil {
		ctx = context.Background()
	}
	attemptTimeout := min(c.cfg.Timeout, 30*time.Second)
	totalTimeout := attemptTimeout*time.Duration(transport.DefaultNetworkRetries+1) + transport.DefaultNetworkRetryDelay*time.Duration(transport.DefaultNetworkRetries)
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(string(requestBody)))
	if err != nil {
		return errors.New("invalid Sub2API request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	// Sub2API's admin API uses x-api-key. Never place the key in a URL, body,
	// error, or persisted metadata.
	req.Header.Set("x-api-key", c.cfg.APIKey)
	for key, values := range options.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	request := transport.Request{Method: method, URL: u.String(), Headers: req.Header, Body: requestBody, Timeout: attemptTimeout, RetryBeforeSend: method != http.MethodGet && method != http.MethodHead}
	// Reads can recover after a lost response. Mutations retry only when the
	// transport proves that no connection was established for the request.
	responseLimit := int64(16 << 20)
	if out == nil {
		responseLimit = 0
	}
	resp, err := transport.DoHTTPWithRetry(ctx, c.http, request, responseLimit, transport.DefaultNetworkRetries, transport.DefaultNetworkRetryDelay, c.cfg.OnRetry)
	if err != nil {
		// Keep transport evidence and the cancellation sentinel together. A
		// deadline while dialing must remain distinguishable from a sent write
		// whose result is unknown.
		return fmt.Errorf("Sub2API request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &RemoteError{StatusCode: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	b := resp.Body
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("Sub2API returned invalid JSON")
	}
	return nil
}
func unwrapDataList(v any) any {
	if m, ok := v.(map[string]any); ok {
		if x, ok := m["data"].([]any); ok {
			return x
		}
	}
	return v
}
func (c *Client) Groups(ctx context.Context) (any, error) {
	var v any
	e := c.request(ctx, http.MethodGet, "api/v1/admin/groups/all?platform=openai", nil, &v)
	return sanitizeGroups(unwrapDataList(v)), e
}
func (c *Client) Proxies(ctx context.Context) (any, error) {
	var v any
	e := c.request(ctx, http.MethodGet, "api/v1/admin/proxies/all", nil, &v)
	return sanitizeProxies(unwrapDataList(v)), e
}

func sanitizeGroups(v any) []map[string]any {
	out := []map[string]any{}
	for _, item := range asItems(v) {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := intValue(m["id"])
		if id <= 0 {
			continue
		}
		name := stringValue(m["name"])
		if name == "" {
			name = fmt.Sprintf("号池 %d", id)
		}
		status := stringValue(m["status"])
		if status == "" {
			status = "active"
		}
		out = append(out, map[string]any{"id": id, "name": name, "status": status})
	}
	return out
}
func sanitizeProxies(v any) []map[string]any {
	out := []map[string]any{}
	for _, item := range asItems(v) {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := intValue(m["id"])
		if id <= 0 {
			continue
		}
		name, status := stringValue(m["name"]), stringValue(m["status"])
		if name == "" {
			name = fmt.Sprintf("代理 %d", id)
		}
		if status == "" {
			status = "active"
		}
		out = append(out, map[string]any{"id": id, "name": name, "protocol": stringValue(m["protocol"]), "host": stringValue(m["host"]), "port": intValue(m["port"]), "ipAddress": stringValue(m["ip_address"]), "status": status})
	}
	return out
}
func asItems(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		if a, ok := x["data"].([]any); ok {
			return a
		}
	}
	return nil
}
func (c *Client) Upload(ctx context.Context, payload any) (any, error) {
	return c.UploadWithOptions(ctx, payload, NewIdempotencyKey())
}

// UploadWithOptions updates existing OpenAI OAuth authorizations and imports
// only accounts that are absent after a complete remote lookup. The key is
// copied only to the new-account batch request, never from arbitrary headers.
func (c *Client) UploadWithOptions(ctx context.Context, payload any, idempotencyKey string) (any, error) {
	return c.upsertOAuthAccounts(ctx, payload, idempotencyKey)
}

func validHeaderToken(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n")
}
func (c *Client) Account(ctx context.Context, id string) (any, error) {
	var v any
	e := c.request(ctx, http.MethodGet, "api/v1/admin/accounts/"+url.PathEscape(id), nil, &v)
	return v, e
}

// AccountPublic is safe for UI responses and logs. Use Account only inside the
// repair hook when the private credentials are actually needed for an update.
func (c *Client) AccountPublic(ctx context.Context, id string) (map[string]any, error) {
	v, e := c.Account(ctx, id)
	if e != nil {
		return nil, e
	}
	decoded := DecodeRemoteAccounts(v)
	if len(decoded) == 0 {
		return nil, errors.New("Sub2API account response missing id")
	}
	return decoded[0].Public(), nil
}
func (c *Client) Update(ctx context.Context, id string, payload any) (any, error) {
	var v any
	e := c.request(ctx, http.MethodPut, "api/v1/admin/accounts/"+url.PathEscape(id), payload, &v)
	return v, e
}

// ErrorAccounts reads all pages of OpenAI accounts currently marked error.
// The response is normalized so old and new Sub2API deployments are both
// accepted.
func (c *Client) ErrorAccounts(ctx context.Context, groupIDs []int) ([]map[string]any, error) {
	return c.openAIAccounts(ctx, groupIDs, true)
}

// OpenAIAccounts includes healthy records so subscription exclusions also
// protect another record sharing an email with an error record.
func (c *Client) OpenAIAccounts(ctx context.Context) ([]map[string]any, error) {
	return c.openAIAccounts(ctx, nil, false)
}

func (c *Client) openAIAccounts(ctx context.Context, groupIDs []int, errorsOnly bool) ([]map[string]any, error) {
	out := []map[string]any{}
	for page := 1; page <= 1000; page++ {
		path := fmt.Sprintf("api/v1/admin/accounts?page=%d&page_size=100&platform=openai", page)
		if errorsOnly {
			path += "&status=error"
		}
		var raw any
		if e := c.request(ctx, http.MethodGet, path, nil, &raw); e != nil {
			return nil, e
		}
		data := raw
		if m, ok := raw.(map[string]any); ok && m["data"] != nil {
			data = m["data"]
		}
		items := []any{}
		pages := page
		reportedPages := false
		switch x := data.(type) {
		case []any:
			items = x
		case map[string]any:
			if v, ok := x["items"].([]any); ok {
				items = v
			}
			if n := intValue(x["pages"]); n > 0 {
				pages = int(n)
				reportedPages = true
			}
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// Some deployments ignore query filters. Match the legacy client
			// before offering a record to automatic OAuth repair.
			if platform := stringValue(m["platform"]); platform != "" && platform != "openai" {
				continue
			}
			if status := stringValue(m["status"]); errorsOnly && status != "" && status != "error" {
				continue
			}
			if len(groupIDs) > 0 && !inGroups(m, groupIDs) {
				continue
			}
			out = append(out, m)
		}
		if !reportedPages && len(items) >= 100 {
			pages = page + 1
		}
		if page >= pages {
			break
		}
	}
	return out, nil
}
func inGroups(account map[string]any, wanted []int) bool {
	return groupsContain(readGroupIDs(account), wanted)
}
func AccountEmail(account map[string]any) string {
	for _, parent := range []string{"credentials", "extra"} {
		if m, ok := account[parent].(map[string]any); ok {
			if v, ok := m["email"].(string); ok {
				v = strings.ToLower(strings.TrimSpace(v))
				if emailRe.FindString(v) == v && v != "" {
					return v
				}
			}
		}
	}
	// Public monitor summaries contain only a top-level email. For remote
	// accounts, credentials.email and extra.email remain authoritative.
	if v, ok := account["email"].(string); ok {
		v = strings.ToLower(strings.TrimSpace(v))
		if emailRe.FindString(v) == v && v != "" {
			return v
		}
	}
	if v, ok := account["name"].(string); ok {
		return strings.ToLower(emailRe.FindString(v))
	}
	return ""
}
func (c *Client) ClearError(ctx context.Context, id string) error {
	return c.request(ctx, http.MethodPost, "api/v1/admin/accounts/"+url.PathEscape(id)+"/clear-error", map[string]any{}, nil)
}
func (c *Client) SetSchedulable(ctx context.Context, id string, value bool) error {
	return c.request(ctx, http.MethodPost, "api/v1/admin/accounts/"+url.PathEscape(id)+"/schedulable", map[string]any{"schedulable": value}, nil)
}

func (c *Client) Accounts(ctx context.Context) (any, error) {
	var v any
	e := c.request(ctx, http.MethodGet, "api/v1/admin/accounts", nil, &v)
	return v, e
}

// ValidateBaseURL exposes the same validation used before every admin request.
func ValidateBaseURL(raw string) (string, error) { return normalizeBaseURL(raw) }
