package sub2api

import (
	"errors"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// ParseUploadSettings validates shared upload settings before any account mutation.
// The returned key is for this process only; job metadata has no config field.
func ParseUploadSettings(value any) (Config, UploadOptions, error) {
	var cfg Config
	var opts UploadOptions
	m, ok := value.(map[string]any)
	if !ok {
		return cfg, opts, errors.New("自动上传需要有效的 Sub2API 设置")
	}
	base, _ := m["baseUrl"].(string)
	key, _ := m["adminApiKey"].(string)
	if strings.TrimSpace(key) == "" {
		key, _ = m["apiKey"].(string)
	}
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return cfg, opts, errors.New("自动上传需要有效的 Sub2API 地址")
	}
	if strings.TrimSpace(key) == "" || len(key) > 512 || strings.ContainsAny(key, "\r\n") {
		return cfg, opts, errors.New("自动上传需要有效的 Sub2API 管理密钥")
	}
	cfg.BaseURL, cfg.APIKey = strings.TrimRight(u.String(), "/"), key
	integer := func(v any, blank bool) (*int, error) {
		var n int
		switch x := v.(type) {
		case nil:
			return nil, nil
		case string:
			if strings.TrimSpace(x) == "" && blank {
				return nil, nil
			}
			n, err = strconv.Atoi(strings.TrimSpace(x))
			if err != nil {
				return nil, errors.New("Sub2API 数值选项必须为整数")
			}
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) || x != math.Trunc(x) || x < 0 || x > 9007199254740991 {
				return nil, errors.New("Sub2API 数值选项必须为有效整数")
			}
			n = int(x)
		case int:
			n = x
		default:
			return nil, errors.New("Sub2API 数值选项必须为整数")
		}
		if n < 0 {
			return nil, errors.New("Sub2API 数值选项必须为有效整数")
		}
		return &n, nil
	}
	for field, dest := range map[string]**int{"concurrency": &opts.Concurrency, "loadFactor": &opts.LoadFactor, "priority": &opts.Priority} {
		if *dest, err = integer(m[field], true); err != nil {
			return cfg, opts, err
		}
	}
	if n, e := integer(m["proxyId"], true); e != nil {
		return cfg, opts, e
	} else if n != nil {
		opts.ProxyID = *n
	}
	if raw := m["groupIds"]; raw != nil {
		var list []any
		switch x := raw.(type) {
		case []any:
			list = x
		case []string:
			for _, id := range x {
				list = append(list, id)
			}
		case []int:
			for _, id := range x {
				list = append(list, id)
			}
		default:
			return cfg, opts, errors.New("Sub2API 分组必须为 ID 列表")
		}
		seen := map[int]bool{}
		for _, id := range list {
			n, e := integer(id, false)
			if e != nil || n == nil || *n <= 0 {
				return cfg, opts, errors.New("Sub2API 分组 ID 必须为正整数")
			}
			if !seen[*n] {
				opts.GroupIDs = append(opts.GroupIDs, *n)
				seen[*n] = true
			}
		}
	}
	if raw := m["modelWhitelist"]; raw != nil {
		switch raw.(type) {
		case string, []string:
		case []any:
			for _, v := range raw.([]any) {
				if _, ok := v.(string); !ok {
					return cfg, opts, errors.New("Sub2API 模型列表必须为文本")
				}
			}
		default:
			return cfg, opts, errors.New("Sub2API 模型列表必须为文本")
		}
		opts.ModelWhitelist = NormalizeModelWhitelist(raw)
	}
	mode, exists := m["codexFingerprintMode"]
	if exists {
		var ok bool
		opts.CodexFingerprintMode, ok = mode.(string)
		if !ok {
			return cfg, opts, errors.New("invalid Codex fingerprint mode")
		}
		opts.CodexFingerprintMode = strings.ToLower(strings.TrimSpace(opts.CodexFingerprintMode))
	}
	if opts.CodexFingerprintMode == "" {
		opts.CodexFingerprintMode = "session"
	}
	if raw, exists := m["openaiRequestTimezone"]; exists {
		var ok bool
		opts.OpenAIRequestTimezone, ok = raw.(string)
		if !ok {
			return cfg, opts, errors.New("OpenAI 请求时区必须为文本")
		}
	}
	opts.OpenAIRequestTimezone, err = NormalizeOpenAIRequestTimezone(opts.OpenAIRequestTimezone)
	if err != nil {
		return cfg, opts, err
	}
	if err := opts.Validate(); err != nil {
		return cfg, opts, err
	}
	cfg.GroupIDs, cfg.CodexFingerprintMode = opts.GroupIDs, opts.CodexFingerprintMode
	cfg.OpenAIRequestTimezone = opts.OpenAIRequestTimezone
	return cfg, opts, nil
}

// NormalizeModelWhitelist preserves the accepted text/list forms and stable order.
func NormalizeModelWhitelist(v any) []string {
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
