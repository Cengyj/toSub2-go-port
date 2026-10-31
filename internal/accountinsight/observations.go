package accountinsight

import "tosub2-go-port/internal/protocol"

// Keep the complete earlier observation when a failed subscription lookup only
// supplied matching account metadata and omitted the expiry field. A reported
// null is authoritative, and changed identity, scope or plan cannot borrow an
// earlier date. The caller handles an entirely absent next observation.
func mergeSubscriptionObservation(previous, next *protocol.InsightSubscription, errs []protocol.InsightError) *protocol.InsightSubscription {
	if previous == nil || next == nil || next.Reported || !previous.IdentityVerified || !next.IdentityVerified ||
		previous.AccountID != next.AccountID || previous.Scope != next.Scope || previous.PlanType != next.PlanType {
		return next
	}
	for _, err := range errs {
		if err.Component == "subscription" {
			return previous
		}
	}
	return next
}
