package protocol

import "time"

// InsightCredentials is a transient, immutable view of one committed OAuth grant.
// It must never be persisted, logged, or included in a public response.
type InsightCredentials struct {
	AccessToken         string `json:"-"`
	AccountID           string `json:"-"`
	Email               string `json:"-"`
	AuthorizationMethod string `json:"-"`
	PlanType            string `json:"-"`
}

type InsightsOptions struct {
	IncludeResetCredits bool
	Timezone            string
	SkipComponents      []string
}

// InsightsClient accepts the low-level, manual-redirect transport only. It does
// not use the authentication Client, challenge solver, refresh, or login paths.
type InsightsClient struct{ Transport Doer }

type InsightResult struct {
	Usage        *InsightUsage        `json:"usage,omitempty"`
	BPS          *InsightBPS          `json:"bps,omitempty"`
	Subscription *InsightSubscription `json:"subscription,omitempty"`
	Errors       []InsightError       `json:"errors"`
	// AttemptedComponents records actual transport dispatches, once per component.
	AttemptedComponents []string `json:"-"`
}

type InsightError struct {
	Component       string     `json:"component"`
	Code            string     `json:"code"`
	Message         string     `json:"message"`
	HTTPStatus      int        `json:"httpStatus,omitempty"`
	RetryAfter      *time.Time `json:"retryAfter,omitempty"`
	UpstreamCode    string     `json:"upstreamCode,omitempty"`
	UpstreamType    string     `json:"upstreamType,omitempty"`
	TransportReason string     `json:"transportReason,omitempty"`
}

type InsightUsage struct {
	Source       string               `json:"source"`
	ObservedAt   time.Time            `json:"observedAt"`
	PlanType     string               `json:"planType,omitempty"`
	Allowed      *bool                `json:"allowed"`
	LimitReached *bool                `json:"limitReached"`
	Windows      []InsightWindow      `json:"windows"`
	Features     []InsightFeature     `json:"features"`
	Credits      *InsightCredits      `json:"credits"`
	ResetCredits *InsightResetCredits `json:"resetCredits"`
}

type InsightWindow struct {
	Name          string     `json:"name"`
	WindowSeconds *int64     `json:"windowSeconds"`
	UsedPercent   *float64   `json:"usedPercent"`
	ResetsAt      *time.Time `json:"resetsAt"`
}

type InsightFeature struct {
	Name           string          `json:"name"`
	MeteredFeature string          `json:"meteredFeature,omitempty"`
	Allowed        *bool           `json:"allowed"`
	LimitReached   *bool           `json:"limitReached"`
	Windows        []InsightWindow `json:"windows"`
}

// Balance is upstream Codex credits, never dollars, tokens, or BPS credits.
type InsightCredits struct {
	HasCredits *bool   `json:"hasCredits"`
	Unlimited  *bool   `json:"unlimited"`
	Balance    *string `json:"balance"`
}

type InsightResetCredits struct {
	AvailableCount *int64               `json:"availableCount"`
	Credits        []InsightResetCredit `json:"credits"`
	ObservedAt     time.Time            `json:"observedAt"`
}

type InsightResetCredit struct {
	ExpiresAt *time.Time `json:"expiresAt"`
}

type InsightBPS struct {
	Source     string            `json:"source"`
	ObservedAt time.Time         `json:"observedAt"`
	Allowed    *bool             `json:"allowed"`
	Models     []InsightBPSModel `json:"models"`
}

type InsightBPSModel struct {
	ID               string   `json:"id"`
	Label            string   `json:"label,omitempty"`
	Available        *bool    `json:"available"`
	Restricted       bool     `json:"restricted"`
	ReasoningEfforts []string `json:"reasoningEfforts"`
}

type InsightSubscription struct {
	Source           string    `json:"source"`
	ObservedAt       time.Time `json:"observedAt"`
	PlanType         string    `json:"planType,omitempty"`
	ExpiresAt        *string   `json:"expiresAt"`
	Reported         bool      `json:"reported"`
	Scope            string    `json:"scope"`
	AccountID        string    `json:"accountId"`
	IdentityVerified bool      `json:"identityVerified"`
}
