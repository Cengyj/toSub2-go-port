package providers

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"tosub2-go-port/internal/mail"
)

var lubanCodeFieldRE = regexp.MustCompile(`(?i)^(?:sms_?)?(?:code|otp|verification_code)$`)
var lubanTimeFieldRE = regexp.MustCompile(`(?i)(created|received|delivered|sent|arrival|timestamp|date|time|updated)(_?at|_?on|_?time)?$`)

type lubanCodeCandidate struct {
	code  string
	score int
}

// freshLubanCode preserves the legacy sms_code priority while using available
// message timestamps to reject old SMS. Responses without timestamps remain
// valid because the provider's request_id itself identifies the leased order.
func freshLubanCode(data map[string]any, cutoff int64) (string, bool) {
	cleaned := cleanLubanPayload(data)
	bytes, err := json.Marshal(lubanExtractionPayload(cleaned))
	if err != nil {
		return "", false
	}
	all := mail.ExtractCandidates(string(bytes))
	fresh := mail.FilterByRequestTime(all, cutoff)
	if len(fresh) == 0 {
		return "", len(all) > 0
	}
	ranked := []lubanCodeCandidate{}
	for _, field := range []struct {
		name  string
		score int
	}{{"sms_code", 100}, {"sms_msg", 20}, {"message", 20}, {"text", 20}, {"msg", 0}} {
		collectLubanCodes(data[field.name], field.score, field.name, &ranked)
	}
	collectLubanCodes(cleaned, -20, "root", &ranked)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	for _, priority := range ranked {
		for _, candidate := range fresh {
			if candidate.Code == priority.code {
				return priority.code, false
			}
		}
	}
	return fresh[0].Code, false
}

// Luban accepts six-digit codes in any message field, without requiring
// mailbox-specific vocabulary. Wrapping scalar text gives the mail parser
// that same behavior while retaining inherited message timestamps.
func lubanExtractionPayload(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, value := range x {
			if lubanTimeFieldRE.MatchString(key) {
				out[key] = value
			} else {
				out[key] = lubanExtractionPayload(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = lubanExtractionPayload(value)
		}
		return out
	case string, float64, json.Number:
		return map[string]any{"sms_code": x}
	default:
		return v
	}
}

func collectLubanCodes(v any, base int, key string, out *[]lubanCodeCandidate) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for name := range x {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			if !lubanTimeFieldRE.MatchString(name) {
				collectLubanCodes(x[name], base, name, out)
			}
		}
	case []any:
		for _, value := range x {
			collectLubanCodes(value, base, key, out)
		}
	default:
		text := strings.TrimSpace(valueString(v))
		code := ExtractCode(text)
		if code == "" {
			return
		}
		score := base
		if lubanCodeFieldRE.MatchString(key) {
			score += 100
		}
		if code == text {
			score += 40
		}
		*out = append(*out, lubanCodeCandidate{code: code, score: score})
	}
}
func cleanLubanPayload(v any) any {
	switch x := v.(type) {
	case map[string]any:
		result := map[string]any{}
		for k, value := range x {
			low := strings.ToLower(strings.ReplaceAll(k, "_", ""))
			if low == "requestid" || low == "applicationid" || low == "countryid" || low == "serviceid" || low == "id" || low == "number" || low == "phone" || strings.Contains(low, "apikey") || strings.Contains(low, "token") || strings.Contains(low, "password") {
				continue
			}
			result[k] = cleanLubanPayload(value)
		}
		return result
	case []any:
		out := make([]any, 0, len(x))
		for _, value := range x {
			out = append(out, cleanLubanPayload(value))
		}
		return out
	default:
		return v
	}
}
