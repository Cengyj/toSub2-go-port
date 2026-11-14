package protocol

import "context"

// QuerySubscription reads the selected account's current subscription using
// only the supplied access token. It never logs in, refreshes that token, or
// dispatches usage, BPS, or reset-credit requests. Known token expiry is the
// caller's admission check; an upstream rejection is returned without fallback
// authentication.
//
// A saved plan is historical metadata: an upgrade or downgrade must not be
// mistaken for an identity mismatch. Account and email binding, and consistency
// between the two live subscription sources, still use Query's strict checks.
// The caller owns committing a verified observation and retaining any warnings.
func (c *InsightsClient) QuerySubscription(ctx context.Context, creds InsightCredentials, options InsightsOptions) InsightResult {
	creds.PlanType = "unknown"
	options.IncludeResetCredits = false
	options.SkipComponents = []string{"usage", "bps", "resetCredits"}
	options.subscriptionRefresh = true
	return c.Query(ctx, creds, options)
}
