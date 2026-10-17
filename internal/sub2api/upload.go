package sub2api

import (
	"errors"
	"strings"
)

// PublicUploadResult allows only aggregate numbers, booleans and IDs. Some
// deployments echo imported account credentials, so raw upload responses must
// never be returned by the console.
func PublicUploadResult(raw any) map[string]any {
	out := map[string]any{}
	m, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for _, k := range []string{"ok", "success"} {
		if v, ok := m[k].(bool); ok {
			out[k] = v
		}
	}
	for _, k := range []string{"success", "created", "updated", "imported", "failed", "skipped", "unknown", "not_attempted", "total", "count", "success_count", "failure_count"} {
		if v, ok := m[k].(float64); ok {
			out[k] = v
		}
	}
	for _, k := range []string{"ids", "account_ids"} {
		if a, ok := m[k].([]any); ok {
			ids := []int64{}
			for _, v := range a {
				if n := intValue(v); n > 0 {
					ids = append(ids, n)
				}
			}
			out[k] = ids
		}
	}
	if d, ok := m["data"]; ok {
		out["data"] = PublicUploadResult(d)
	}
	return out
}

type UploadOptions struct {
	GroupIDs              []int    `json:"groupIds,omitempty"`
	ProxyID               int      `json:"proxyId,omitempty"`
	Concurrency           *int     `json:"concurrency,omitempty"`
	LoadFactor            *int     `json:"loadFactor,omitempty"`
	Priority              *int     `json:"priority,omitempty"`
	ModelWhitelist        []string `json:"modelWhitelist,omitempty"`
	CodexFingerprintMode  string   `json:"codexFingerprintMode,omitempty"`
	OpenAIRequestTimezone string   `json:"openaiRequestTimezone"`
}

func (o UploadOptions) Validate() error {
	if _, err := NormalizeOpenAIRequestTimezone(o.OpenAIRequestTimezone); err != nil {
		return err
	}
	for _, id := range o.GroupIDs {
		if id <= 0 {
			return errors.New("invalid Sub2API group ID")
		}
	}
	if o.ProxyID < 0 {
		return errors.New("invalid Sub2API proxy ID")
	}
	for _, v := range []*int{o.Concurrency, o.LoadFactor, o.Priority} {
		if v != nil && (*v < 0 || *v > 10000) {
			return errors.New("Sub2API numeric option must be between 0 and 10000")
		}
	}
	if len(o.ModelWhitelist) > 200 {
		return errors.New("too many model whitelist entries")
	}
	for _, model := range o.ModelWhitelist {
		if strings.TrimSpace(model) == "" || len(model) > 200 || strings.ContainsAny(model, "\r\n") {
			return errors.New("invalid model whitelist entry")
		}
	}
	switch o.CodexFingerprintMode {
	case "", "off", "device", "session", "full":
	default:
		return errors.New("invalid Codex fingerprint mode")
	}
	return nil
}

// BuildUploadPayload preserves the import account fields sent by the legacy
// client, excluding its local proxy_key reference. Explicit console options
// override saved settings. Credentials are included only in the request body,
// never in public results or monitor state.
func BuildUploadPayload(accounts []map[string]any, o UploadOptions) (map[string]any, error) {
	if e := o.Validate(); e != nil {
		return nil, e
	}
	timezone, _ := NormalizeOpenAIRequestTimezone(o.OpenAIRequestTimezone)
	out := make([]any, 0, len(accounts))
	for _, a := range accounts {
		m := cloneMap(a)
		delete(m, "proxy_key")
		creds := cloneMap(a["credentials"])
		if len(creds) == 0 {
			return nil, errors.New("account lacks OAuth credentials")
		}
		extra := cloneMap(a["extra"])
		if err := normalizeOAuthAuthorization(creds, extra); err != nil {
			return nil, err
		}
		mode := o.CodexFingerprintMode
		if mode == "" {
			mode = "session"
		}
		extra["codex_fingerprint_mode"] = mode
		extra["openai_request_timezone"] = timezone
		if len(o.ModelWhitelist) > 0 {
			mapping := map[string]any{}
			for _, model := range o.ModelWhitelist {
				mapping[model] = model
			}
			creds["model_mapping"] = mapping
		}
		m["credentials"] = creds
		m["extra"] = extra
		m["status"] = "active"
		m["schedulable"] = true
		if len(o.GroupIDs) > 0 {
			m["group_ids"] = append([]int(nil), o.GroupIDs...)
		} else if m["group_ids"] == nil {
			m["group_ids"] = []int{}
		}
		if o.ProxyID > 0 {
			m["proxy_id"] = o.ProxyID
		}
		if o.Concurrency != nil {
			m["concurrency"] = *o.Concurrency
		}
		if o.LoadFactor != nil {
			m["load_factor"] = *o.LoadFactor
		}
		if o.Priority != nil {
			m["priority"] = *o.Priority
		}
		out = append(out, m)
	}
	return map[string]any{"accounts": out}, nil
}
func cloneMap(value any) map[string]any {
	out := map[string]any{}
	if m, ok := value.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
