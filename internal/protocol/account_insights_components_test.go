package protocol

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestInsightsComponentSkipsNeverDispatchSkippedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		skip []string
		want []string
	}{
		{"default", nil, []string{"usage", "bps", "subscription", "resetCredits"}},
		{"usage", []string{"usage"}, []string{"bps", "subscription"}},
		{"bps", []string{"bps"}, []string{"usage", "subscription", "resetCredits"}},
		{"subscription", []string{"subscription"}, []string{"usage", "bps", "resetCredits"}},
		{"resetCredits", []string{"resetCredits"}, []string{"usage", "bps", "subscription"}},
		{"unknown-ignored", []string{"futureComponent", "Usage"}, []string{"usage", "bps", "subscription", "resetCredits"}},
		{"all", []string{"usage", "bps", "subscription", "resetCredits"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []string{}
			client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
				var component, body string
				switch request.URL {
				case insightUsageURL:
					component, body = "usage", `{"rate_limit":null}`
				case insightBPSAccessURL:
					component, body = "bps", `{"allowed":false}`
				case insightAccountsURL:
					component, body = "subscription", insightAccountsFixture("2030-01-01T00:00:00Z")
				case insightResetCreditsURL:
					component, body = "resetCredits", `{"available_count":0}`
				default:
					t.Fatalf("unexpected request: %s", request.URL)
				}
				if slices.Contains(tc.skip, component) {
					t.Fatalf("skipped component dispatched: %s", component)
				}
				calls = append(calls, component)
				return insightResponse(200, body), nil
			})}
			creds := insightTestCredentials()
			creds.AuthorizationMethod = "bps"
			result := client.Query(context.Background(), creds, InsightsOptions{IncludeResetCredits: true, SkipComponents: tc.skip})
			if len(result.Errors) != 0 || !reflect.DeepEqual(calls, tc.want) || !reflect.DeepEqual(result.AttemptedComponents, tc.want) {
				t.Fatalf("component dispatch changed: calls=%v attempted=%v want=%v errors=%+v", calls, result.AttemptedComponents, tc.want, result.Errors)
			}
			if (result.Usage != nil) != slices.Contains(tc.want, "usage") || (result.BPS != nil) != slices.Contains(tc.want, "bps") || (result.Subscription != nil) != slices.Contains(tc.want, "subscription") {
				t.Fatal("skipped component manufactured a result")
			}
			if result.Usage != nil && (result.Usage.ResetCredits != nil) != slices.Contains(tc.want, "resetCredits") {
				t.Fatal("skipped reset detail manufactured a result")
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(strings.ToLower(string(encoded)), "attemptedcomponents") {
				t.Fatal("internal dispatch bookkeeping entered public JSON")
			}
		})
	}
}

func TestInsightsUsageCooldownLeavesHealthyBPSAvailable(t *testing.T) {
	creds := insightTestCredentials()
	creds.AuthorizationMethod = "bps"
	calls := 0
	client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		calls++
		if request.URL != insightBPSAccessURL {
			t.Fatal("usage cooldown still dispatched quota, reset credits, or subscription")
		}
		return insightResponse(200, `{"allowed":true,"model_catalog":{"models":[]}}`), nil
	})}
	result := client.Query(context.Background(), creds, InsightsOptions{IncludeResetCredits: true, SkipComponents: []string{"usage", "subscription"}})
	if calls != 1 || len(result.Errors) != 0 || result.BPS == nil || result.BPS.Allowed == nil || !*result.BPS.Allowed || result.Usage != nil || !reflect.DeepEqual(result.AttemptedComponents, []string{"bps"}) {
		t.Fatalf("healthy BPS was blocked by quota cooldown: %+v", result)
	}
}

func TestInsightsAttemptedComponentsRecordActualDispatchOnly(t *testing.T) {
	t.Run("cancelled-before-dispatch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
			t.Fatal("pre-cancelled query dispatched")
			return nil, nil
		})}
		result := client.Query(ctx, insightTestCredentials(), InsightsOptions{IncludeResetCredits: true})
		if len(result.AttemptedComponents) != 0 {
			t.Fatalf("unattempted endpoints recorded: %v", result.AttemptedComponents)
		}
	})
	t.Run("later-deadline-exhausted", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		client := &InsightsClient{Transport: insightFixtureDoer(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
			if request.URL != insightUsageURL {
				t.Fatal("budget exhausted yet another endpoint dispatched")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})}
		creds := insightTestCredentials()
		creds.AuthorizationMethod = "bps"
		result := client.Query(ctx, creds, InsightsOptions{IncludeResetCredits: true})
		if !reflect.DeepEqual(result.AttemptedComponents, []string{"usage"}) {
			t.Fatalf("budget failure fabricated attempts: %v", result.AttemptedComponents)
		}
	})
	t.Run("retry-and-subscription-fallback-count-once", func(t *testing.T) {
		usageCalls, subscriptionCalls := 0, 0
		client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
			switch request.URL {
			case insightUsageURL:
				usageCalls++
				if usageCalls == 1 {
					return nil, io.ErrUnexpectedEOF
				}
				return insightResponse(403, `{"error":{"code":"insufficient_scope"}}`), nil
			case insightAccountsURL:
				subscriptionCalls++
				return insightResponse(200, insightAccountsFixture(nil)), nil
			case insightSubscriptionsURL + "?account_id=workspace":
				subscriptionCalls++
				return insightResponse(200, `{"plan_type":"plus","active_until":null}`), nil
			default:
				t.Fatal("unexpected request")
				return nil, nil
			}
		})}
		result := client.Query(context.Background(), insightTestCredentials(), InsightsOptions{IncludeResetCredits: true})
		if usageCalls != 2 || subscriptionCalls != 2 || !reflect.DeepEqual(result.AttemptedComponents, []string{"usage", "subscription"}) {
			t.Fatalf("retry/fallback duplicated component or failed usage dispatched reset detail: %+v", result)
		}
	})
}
