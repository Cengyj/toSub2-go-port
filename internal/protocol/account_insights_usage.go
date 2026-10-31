package protocol

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
)

var insightDecimal = regexp.MustCompile("^-?[0-9]+(?:\\.[0-9]+)?$")

func parseInsightUsage(body []byte, creds InsightCredentials, now time.Time) (*InsightUsage, error) {
	f, err := insightObject(body)
	if err != nil {
		return nil, err
	}
	if err := insightIdentity(f, creds); err != nil {
		return nil, err
	}
	recognized := false
	for _, key := range []string{"rate_limit", "additional_rate_limits", "credits", "rate_limit_reset_credits", "plan_type"} {
		if _, exists := f[key]; exists {
			recognized = true
		}
	}
	if !recognized {
		return nil, errInsightProtocol
	}
	usage := &InsightUsage{Source: "codex_wham", ObservedAt: now, Windows: []InsightWindow{}, Features: []InsightFeature{}}
	if plan, err := insightField[string](f, "plan_type"); err != nil {
		return nil, err
	} else if plan != nil {
		usage.PlanType = accountplan.Normalize(*plan)
	}
	limit, err := insightNested(f, "rate_limit")
	if err != nil {
		return nil, err
	}
	if limit != nil {
		usage.Allowed, usage.LimitReached, usage.Windows, err = parseInsightRateLimit(limit, now)
		if err != nil {
			return nil, err
		}
	}
	features, err := insightField[[]json.RawMessage](f, "additional_rate_limits")
	if err != nil {
		return nil, err
	}
	if features != nil {
		if len(*features) > 128 {
			return nil, errInsightProtocol
		}
		for _, raw := range *features {
			feature, err := insightObject(raw)
			if err != nil {
				return nil, err
			}
			name, err := insightField[string](feature, "limit_name")
			if err != nil {
				return nil, err
			}
			metered, err := insightField[string](feature, "metered_feature")
			if err != nil {
				return nil, err
			}
			item := InsightFeature{Windows: []InsightWindow{}}
			if name != nil {
				if !insightSafeText(*name, 128) {
					return nil, errInsightProtocol
				}
				item.Name = *name
			}
			if metered != nil {
				if !insightSafeText(*metered, 128) {
					return nil, errInsightProtocol
				}
				item.MeteredFeature = *metered
			}
			limit, err := insightNested(feature, "rate_limit")
			if err != nil {
				return nil, err
			}
			if limit != nil {
				item.Allowed, item.LimitReached, item.Windows, err = parseInsightRateLimit(limit, now)
				if err != nil {
					return nil, err
				}
			}
			usage.Features = append(usage.Features, item)
		}
	}
	credits, err := insightNested(f, "credits")
	if err != nil {
		return nil, err
	}
	if credits != nil {
		item := &InsightCredits{}
		if item.HasCredits, err = insightField[bool](credits, "has_credits"); err != nil {
			return nil, err
		}
		if item.Unlimited, err = insightField[bool](credits, "unlimited"); err != nil {
			return nil, err
		}
		if item.Balance, err = insightField[string](credits, "balance"); err != nil {
			return nil, err
		}
		if item.Balance != nil && (len(*item.Balance) > 128 || !insightDecimal.MatchString(*item.Balance)) {
			return nil, errInsightProtocol
		}
		usage.Credits = item
	}
	if raw, exists := f["rate_limit_reset_credits"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		usage.ResetCredits, err = parseInsightResetDetails(raw, now)
		if err != nil {
			return nil, err
		}
	}
	return usage, nil
}

func insightNested(f map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	raw, exists := f[key]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	return insightObject(raw)
}

func parseInsightRateLimit(f map[string]json.RawMessage, now time.Time) (*bool, *bool, []InsightWindow, error) {
	allowed, err := insightField[bool](f, "allowed")
	if err != nil {
		return nil, nil, nil, err
	}
	reached, err := insightField[bool](f, "limit_reached")
	if err != nil {
		return nil, nil, nil, err
	}
	windows := []InsightWindow{}
	for _, name := range []string{"primary_window", "secondary_window"} {
		w, err := insightNested(f, name)
		if err != nil {
			return nil, nil, nil, err
		}
		if w == nil {
			continue
		}
		item := InsightWindow{Name: strings.TrimSuffix(name, "_window")}
		if item.WindowSeconds, err = insightField[int64](w, "limit_window_seconds"); err != nil {
			return nil, nil, nil, err
		}
		if item.WindowSeconds != nil && *item.WindowSeconds < 0 {
			return nil, nil, nil, errInsightProtocol
		}
		if item.UsedPercent, err = insightField[float64](w, "used_percent"); err != nil {
			return nil, nil, nil, err
		}
		if item.UsedPercent != nil && (math.IsInf(*item.UsedPercent, 0) || math.IsNaN(*item.UsedPercent)) {
			return nil, nil, nil, errInsightProtocol
		}
		at, err := insightField[int64](w, "reset_at")
		if err != nil {
			return nil, nil, nil, err
		}
		after, err := insightField[int64](w, "reset_after_seconds")
		if err != nil {
			return nil, nil, nil, err
		}
		if at != nil && (*at < 0 || *at > 253402300799) || after != nil && (*after < 0 || *after > 315576000) {
			return nil, nil, nil, errInsightProtocol
		}
		if at != nil && *at > 0 {
			value := time.Unix(*at, 0).UTC()
			item.ResetsAt = &value
		} else if after != nil {
			value := now.Add(time.Duration(*after) * time.Second)
			item.ResetsAt = &value
		}
		windows = append(windows, item)
	}
	return allowed, reached, windows, nil
}

// Reset identifiers are deliberately discarded: this feature can never consume a credit.
func parseInsightResetDetails(body []byte, now time.Time) (*InsightResetCredits, error) {
	if !insightValidJSON(body) {
		return nil, errInsightProtocol
	}
	result := &InsightResetCredits{ObservedAt: now}
	var rawList []json.RawMessage
	listPresent := false
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if json.Unmarshal(trimmed, &rawList) != nil {
			return nil, errInsightProtocol
		}
		listPresent = true
	} else {
		f, err := insightObject(body)
		if err != nil {
			return nil, err
		}
		recognized := false
		for _, key := range []string{"available_count", "availableCount"} {
			raw, exists := f[key]
			recognized = recognized || exists
			if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			var n int64
			if json.Unmarshal(raw, &n) != nil {
				var value string
				if json.Unmarshal(raw, &value) != nil {
					return nil, errInsightProtocol
				}
				parsed, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil, errInsightProtocol
				}
				n = parsed
			}
			if n < 0 || result.AvailableCount != nil && *result.AvailableCount != n {
				return nil, errInsightProtocol
			}
			result.AvailableCount = &n
		}
		for _, key := range []string{"credits", "rate_limit_reset_credits", "items", "data"} {
			raw, exists := f[key]
			recognized = recognized || exists
			if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			if listPresent {
				return nil, errInsightProtocol
			}
			if json.Unmarshal(raw, &rawList) != nil {
				return nil, errInsightProtocol
			}
			listPresent = true
		}
		if !recognized {
			return nil, errInsightProtocol
		}
	}
	if len(rawList) > 1024 {
		return nil, errInsightProtocol
	}
	if listPresent {
		result.Credits = []InsightResetCredit{}
	}
	for _, raw := range rawList {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		f, err := insightObject(raw)
		if err != nil {
			return nil, err
		}
		kind, err := insightAliasedString(f, "reset_type", "resetType")
		if err != nil {
			return nil, err
		}
		if kind != nil && *kind != "" && *kind != "codex_rate_limits" {
			continue
		}
		status, err := insightField[string](f, "status")
		if err != nil {
			return nil, err
		}
		if status != nil && *status != "" && *status != "available" {
			continue
		}
		expires, err := insightAliasedString(f, "expires_at", "expiresAt")
		if err != nil {
			return nil, err
		}
		item := InsightResetCredit{}
		if expires != nil && strings.TrimSpace(*expires) != "" {
			at, err := time.Parse(time.RFC3339, *expires)
			if err != nil {
				return nil, errInsightProtocol
			}
			at = at.UTC()
			item.ExpiresAt = &at
		}
		result.Credits = append(result.Credits, item)
	}
	if result.AvailableCount == nil && listPresent {
		n := int64(len(result.Credits))
		result.AvailableCount = &n
	}
	return result, nil
}

func insightAliasedString(f map[string]json.RawMessage, keys ...string) (*string, error) {
	var value *string
	for _, key := range keys {
		v, err := insightField[string](f, key)
		if err != nil {
			return nil, err
		}
		if v != nil {
			if value != nil && *value != *v {
				return nil, errInsightProtocol
			}
			value = v
		}
	}
	return value, nil
}

func insightSafeText(value string, max int) bool {
	if len(value) > max {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
