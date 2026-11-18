package protocol

import (
	"fmt"
	"strings"
)

const (
	totpStateUnknown  = "unknown"
	totpStateDisabled = "disabled"
	totpStateEnabled  = "enabled"
)

// totpLiveState describes only a response fetched for the web account already
// verified by currentTOTPAuthorization. An unknown response cannot identify a
// removal target. The reset workflow separately gates a single initial enrollment
// attempt; factors are returned only for one unambiguous active TOTP.
type totpLiveState struct {
	State   string
	Factors []map[string]any
	Summary string
}

// Verification already has an enrollment-bound ID. Parse the full typed list
// without ever passing these members to the removal-target selector.
func verificationTOTPState(info map[string]any) totpLiveState {
	s := classifyTOTPState(info)
	if s.State != totpStateUnknown || info["mfa_enabled_v2"] != true || totpResponseRejected(info) || !totpOutcomeFieldsValid(info) {
		return s
	}
	list, _ := payload(info["factors"])["totp"].([]any)
	seen := make(map[string]bool, len(list))
	var ordinary []map[string]any
	for _, item := range list {
		f := payload(item)
		id, ok := f["id"].(string)
		recovery, typed := f["is_recovery"].(bool)
		if !ok || strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) || seen[id] || !typed || f["factor_type"] != "totp" {
			return s
		}
		seen[id] = true
		if !recovery {
			ordinary = append(ordinary, f)
		}
	}
	if len(ordinary) > 0 {
		s.State, s.Factors = totpStateEnabled, ordinary
	}
	return s
}

func (s totpLiveState) matches(id string) bool {
	if id == "" || s.State != totpStateEnabled {
		return false
	}
	matches := 0
	for _, f := range s.Factors {
		if f["id"] == id && f["factor_type"] == "totp" && f["is_recovery"] == false {
			matches++
		}
	}
	return matches == 1
}

// classifyTOTPState accepts the upstream's compact disabled-state response as
// well as an explicit empty TOTP list. A missing list alone is not proof that a
// factor was removed: a compact response must explicitly report MFA disabled.
// A valid empty array also preserves the original disabled-list contract. The
// diagnostic contains structural information only, never response values such
// as factor IDs, credentials, tokens, or upstream error bodies.
func classifyTOTPState(info map[string]any) totpLiveState {
	state := totpLiveState{State: totpStateUnknown}
	enabledValue, enabledPresent := info["mfa_enabled_v2"]
	enabled, enabledValid := enabledValue.(bool)
	factorsValue, factorsPresent := info["factors"]
	factors := payload(factorsValue)
	totpValue, totpPresent := factors["totp"]
	all, listValid := totpValue.([]any)
	ordinary, ordinaryValid := ordinaryTOTPFactors(info)
	flag := "absent"
	if enabledPresent {
		flag = "invalid"
		if enabledValid {
			flag = fmt.Sprint(enabled)
		}
	}
	state.Summary = fmt.Sprintf("mfa_enabled_v2=%s; factors=%s; totp=%s; count=%d; ordinary=%d",
		flag, totpFieldType(factorsValue, factorsPresent), totpFieldType(totpValue, totpPresent), len(all), len(ordinary))
	if totpResponseRejected(info) || !totpOutcomeFieldsValid(info) {
		return state
	}
	// An invalid factor container is not equivalent to an omitted/null one.
	if factorsPresent && factorsValue != nil && factors == nil {
		return state
	}
	if !enabledPresent && listValid && len(all) == 0 {
		state.State = totpStateDisabled
		return state
	}
	if !enabledPresent || !enabledValid {
		return state
	}
	if !enabled {
		if !totpPresent || totpValue == nil || listValid && len(all) == 0 {
			state.State = totpStateDisabled
		}
		return state
	}
	if !ordinaryValid || !listValid || len(all) != 1 || len(ordinary) != 1 {
		return state
	}
	// ordinaryTOTPFactors requires an ID, the TOTP type and a false recovery
	// flag. Keep the identity a string; never coerce arbitrary response values.
	if id, valid := ordinary[0]["id"].(string); !valid || strings.TrimSpace(id) == "" {
		return state
	}
	state.State, state.Factors = totpStateEnabled, ordinary
	return state
}

func totpFieldType(value any, present bool) string {
	if !present {
		return "absent"
	}
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any, authPayload:
		return "object"
	case []any:
		return "array"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64, float32, int, int64:
		return "number"
	default:
		return "invalid"
	}
}
