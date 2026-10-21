package accountplan

import "strings"

// Excluded matches only a recognized selection or the explicit unknown option.
func Excluded(plan string, selected []string) bool {
	plan = Normalize(plan)
	for _, value := range selected {
		normalized := Normalize(value)
		if normalized == "unknown" && !strings.EqualFold(strings.TrimSpace(value), "unknown") {
			continue
		}
		if normalized == plan {
			return true
		}
	}
	return false
}

// AutomaticRepairExcluded gives every known subscription source a veto.
// Unknown remote metadata falls back to local metadata, never to Free.
func AutomaticRepairExcluded(local string, remote []string, excluded []string) bool {
	local = Normalize(local)
	if local != "unknown" && Excluded(local, excluded) {
		return true
	}
	if len(remote) == 0 {
		return Excluded(local, excluded)
	}
	for _, value := range remote {
		plan := Normalize(value)
		if plan == "unknown" {
			plan = local
		}
		if Excluded(plan, excluded) {
			return true
		}
	}
	return false
}
