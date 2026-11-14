// Package cpa exports Codex authorizations to CLIProxyAPI's management API.
// The contract is verified against router-for-me/CLIProxyAPI commit
// 1aaf56bc6a0fc6fce19049ba7ae8554c882304a6: auth_files_crud.go,
// handler.go and internal/auth/codex/{token,openai_auth}.go.
package cpa

import (
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	BaseURL string        `json:"baseUrl"`
	APIKey  string        `json:"-"`
	Timeout time.Duration `json:"-"`
	UploadOptions
}

// UploadOptions are applied to every auth file sent by an upload operation.
// Weight is a pointer so an omitted legacy setting defaults to 1 while an
// explicit zero remains excluded from CPA's weighted round-robin selection.
type UploadOptions struct {
	Priority int  `json:"priority"`
	Weight   *int `json:"weight,omitempty"`
}

func (o UploadOptions) EffectiveWeight() int {
	if o.Weight == nil {
		return 1
	}
	return *o.Weight
}

func (o UploadOptions) Validate() error {
	if int64(o.Priority) < math.MinInt32 || int64(o.Priority) > math.MaxInt32 {
		return ErrInvalidPriority
	}
	if o.EffectiveWeight() < 0 || o.EffectiveWeight() > 1_000_000 {
		return ErrInvalidWeight
	}
	return nil
}

// Clone keeps frozen settings independent of subsequent configuration edits.
func (o UploadOptions) Clone() UploadOptions {
	w := o.EffectiveWeight()
	return UploadOptions{Priority: o.Priority, Weight: &w}
}

var (
	ErrInvalidBaseURL  = errors.New("请输入有效的 CPA 服务地址（HTTP 或 HTTPS，不含密钥、查询参数或片段）")
	ErrInvalidAPIKey   = errors.New("请输入有效的 CPA 管理密钥")
	ErrInvalidPriority = errors.New("CPA 优先级须为 -2147483648 到 2147483647 之间的整数")
	ErrInvalidWeight   = errors.New("CPA 权重须为 0 到 1000000 之间的整数")
)

// ValidateBaseURL accepts a server root or its /v0/management URL. A reverse
// proxy's path prefix is preserved. No credentials are accepted in the URL.
func ValidateBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", ErrInvalidBaseURL
	}
	if strings.ContainsAny(u.Path, "\\\r\n\t") {
		return "", ErrInvalidBaseURL
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return "", ErrInvalidBaseURL
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/v0/management/auth-files") {
		return "", ErrInvalidBaseURL
	}
	u.Path = strings.TrimSuffix(u.Path, "/v0/management")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func validateConfig(cfg Config) (Config, error) {
	if err := cfg.UploadOptions.Validate(); err != nil {
		return Config{}, err
	}
	base, err := ValidateBaseURL(cfg.BaseURL)
	if err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.APIKey) == "" || len(cfg.APIKey) > 4096 {
		return Config{}, ErrInvalidAPIKey
	}
	for _, char := range cfg.APIKey {
		if char < 32 || char == 127 {
			return Config{}, ErrInvalidAPIKey
		}
	}
	cfg.BaseURL = base
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return cfg, nil
}

// ParseUploadSettings validates the process-local configuration before any
// write. The management secret is never included when Config is serialized.
func ParseUploadSettings(value any) (Config, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return Config{}, errors.New("推送到 CPA 需要有效的服务设置")
	}
	base, _ := m["baseUrl"].(string)
	key, _ := m["adminApiKey"].(string)
	options := UploadOptions{}
	if value, exists := m["priority"]; exists {
		priority, ok := integerSetting(value)
		if !ok || priority < math.MinInt32 || priority > math.MaxInt32 {
			return Config{}, ErrInvalidPriority
		}
		options.Priority = int(priority)
	}
	if value, exists := m["weight"]; exists {
		weight, ok := integerSetting(value)
		if !ok || weight < 0 || weight > 1_000_000 {
			return Config{}, ErrInvalidWeight
		}
		w := int(weight)
		options.Weight = &w
	}
	return validateConfig(Config{BaseURL: base, APIKey: key, UploadOptions: options.Clone()})
}

func integerSetting(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < math.MinInt32 || v > math.MaxInt32 {
			return 0, false
		}
		return int64(v), true
	default:
		return 0, false
	}
}
