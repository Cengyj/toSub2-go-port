package sub2api

import (
	"errors"
	"strings"

	"tosub2-go-port/internal/accountplan"
)

var ErrPlanExcluded = errors.New("account plan is excluded from automatic relogin")

// NormalizeExcludedPlanTypes returns owned, canonical values. Empty settings
// exclude no accounts; unknown accounts are excluded only by an explicit
// "unknown" selection, never by a misspelled setting.
func NormalizeExcludedPlanTypes(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		plan := accountplan.Normalize(trimmed)
		if plan == "unknown" && !strings.EqualFold(trimmed, "unknown") {
			continue
		}
		if !seen[plan] {
			seen[plan] = true
			out = append(out, plan)
		}
	}
	return out
}

func PlanExcluded(plan string, excluded []string) bool {
	plan = accountplan.Normalize(plan)
	for _, value := range excluded {
		if accountplan.Normalize(value) == plan {
			return true
		}
	}
	return false
}

func AccountPlanType(account map[string]any) string {
	return accountplan.FromAccount(account)
}
