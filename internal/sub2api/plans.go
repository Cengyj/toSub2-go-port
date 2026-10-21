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
	return accountplan.Excluded(plan, excluded)
}

// AutomaticRepairPlanExcluded gives every known subscription source a veto.
// A stale paid remote value must not override a locally observed excluded tier.
// Unknown still uses the local fallback and is never inferred to be Free.
func AutomaticRepairPlanExcluded(local string, remote []string, excluded []string) bool {
	return accountplan.AutomaticRepairExcluded(local, remote, excluded)
}

func AccountPlanType(account map[string]any) string {
	return accountplan.FromAccount(account)
}
