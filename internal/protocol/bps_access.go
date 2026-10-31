package protocol

import (
	"encoding/json"
	"regexp"
	"time"
)

var insightBPSModelID = regexp.MustCompile("^gpt-[a-zA-Z0-9]+(?:[._-][a-zA-Z0-9]+)*$")

func parseInsightBPS(body []byte, creds InsightCredentials, now time.Time) (*InsightBPS, error) {
	f, err := insightObject(body)
	if err != nil {
		return nil, err
	}
	if err := insightIdentity(f, creds); err != nil {
		return nil, err
	}
	allowed, err := insightField[bool](f, "allowed")
	if err != nil {
		return nil, err
	}
	if _, exists := f["allowed"]; !exists {
		return nil, errInsightProtocol
	}
	result := &InsightBPS{Source: "bps_access", ObservedAt: now, Allowed: allowed, Models: []InsightBPSModel{}}
	catalog, err := insightNested(f, "model_catalog")
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		if allowed != nil && *allowed {
			return nil, errInsightProtocol
		}
		return result, nil
	}
	restricted, err := insightField[[]string](catalog, "restricted_models")
	if err != nil {
		return nil, err
	}
	deny := map[string]bool{}
	if restricted != nil {
		if len(*restricted) > 1024 {
			return nil, errInsightProtocol
		}
		for _, id := range *restricted {
			deny[id] = true
		}
	}
	models, err := insightField[[]json.RawMessage](catalog, "models")
	if err != nil {
		return nil, err
	}
	if models == nil {
		if allowed != nil && *allowed {
			return nil, errInsightProtocol
		}
		return result, nil
	}
	if len(*models) > 1024 {
		return nil, errInsightProtocol
	}
	seen := map[string]bool{}
	for _, raw := range *models {
		model, err := insightObject(raw)
		if err != nil {
			return nil, err
		}
		id, err := insightField[string](model, "id")
		if err != nil {
			return nil, err
		}
		if id == nil || len(*id) > 128 || !insightBPSModelID.MatchString(*id) {
			continue
		}
		if seen[*id] {
			return nil, errInsightProtocol
		}
		seen[*id] = true
		item := InsightBPSModel{ID: *id, Restricted: deny[*id], ReasoningEfforts: []string{}}
		label, err := insightField[string](model, "label")
		if err != nil {
			return nil, err
		}
		if label != nil {
			if !insightSafeText(*label, 160) {
				return nil, errInsightProtocol
			}
			item.Label = *label
		}
		for _, key := range []string{"available", "enabled", "model_picker_enabled"} {
			v, err := insightField[bool](model, key)
			if err != nil {
				return nil, err
			}
			if v != nil {
				if item.Available == nil || !*v {
					item.Available = v
				}
				if !*v {
					item.Restricted = true
				}
			}
		}
		policy, err := insightNested(model, "policy")
		if err != nil {
			return nil, err
		}
		if policy != nil {
			state, err := insightField[string](policy, "state")
			if err != nil {
				return nil, err
			}
			if state != nil && (*state == "disabled" || *state == "unconfigured") {
				item.Restricted = true
			}
		}
		if allowed != nil && !*allowed || item.Restricted {
			no := false
			item.Available = &no
		}
		efforts, err := insightField[[]json.RawMessage](model, "efforts")
		if err != nil {
			return nil, err
		}
		if efforts != nil {
			if len(*efforts) > 16 {
				return nil, errInsightProtocol
			}
			seenEffort := map[string]bool{}
			for _, raw := range *efforts {
				effort, err := insightObject(raw)
				if err != nil {
					return nil, err
				}
				v, err := insightField[string](effort, "value")
				if err != nil {
					return nil, err
				}
				if v == nil {
					continue
				}
				switch *v {
				case "low", "medium", "high", "xhigh":
					if !seenEffort[*v] {
						item.ReasoningEfforts = append(item.ReasoningEfforts, *v)
						seenEffort[*v] = true
					}
				}
			}
		}
		result.Models = append(result.Models, item)
	}
	return result, nil
}
