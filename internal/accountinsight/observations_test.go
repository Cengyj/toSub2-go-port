package accountinsight

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/protocol"
)

func observationSubscriptionFixture(at time.Time) *protocol.InsightSubscription {
	expiry := "2030-01-01T00:00:00Z"
	return &protocol.InsightSubscription{Source: "chatgpt_subscriptions", ObservedAt: at, PlanType: "plus", ExpiresAt: &expiry, Reported: true, Scope: "account", AccountID: "synthetic-workspace", IdentityVerified: true}
}

func TestInsightSubscriptionObservationRetainsOnlyMatchingFailedOmission(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*protocol.InsightSubscription, *protocol.InsightSubscription)
		errors []protocol.InsightError
		keep   bool
	}{
		{"fallback-503", nil, []protocol.InsightError{{Component: "subscription", Code: "upstream_error", HTTPStatus: 503}}, true},
		{"successful-omission", nil, nil, false},
		{"other-component-failed", nil, []protocol.InsightError{{Component: "usage", Code: "network_error"}}, false},
		{"explicit-null", func(_, next *protocol.InsightSubscription) { next.Reported = true }, []protocol.InsightError{{Component: "subscription", Code: "upstream_error"}}, false},
		{"new-reported-date", func(_, next *protocol.InsightSubscription) {
			next.Reported = true
			date := "2031-01-01T00:00:00Z"
			next.ExpiresAt = &date
		}, []protocol.InsightError{{Component: "subscription", Code: "upstream_error"}}, false},
		{"different-account", func(_, next *protocol.InsightSubscription) { next.AccountID = "other-workspace" }, []protocol.InsightError{{Component: "subscription", Code: "upstream_error"}}, false},
		{"different-scope", func(_, next *protocol.InsightSubscription) { next.Scope = "mismatch" }, []protocol.InsightError{{Component: "subscription", Code: "subscription_scope_mismatch"}}, false},
		{"different-plan", func(_, next *protocol.InsightSubscription) { next.PlanType = "team" }, []protocol.InsightError{{Component: "subscription", Code: "subscription_scope_mismatch"}}, false},
		{"next-unverified", func(_, next *protocol.InsightSubscription) { next.IdentityVerified = false }, []protocol.InsightError{{Component: "subscription", Code: "identity_mismatch"}}, false},
		{"previous-unverified", func(previous, _ *protocol.InsightSubscription) { previous.IdentityVerified = false }, []protocol.InsightError{{Component: "subscription", Code: "upstream_error"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := observationSubscriptionFixture(now)
			nextValue := *observationSubscriptionFixture(now.Add(time.Minute))
			nextValue.Source, nextValue.ExpiresAt, nextValue.Reported = "chatgpt_accounts", nil, false
			next := &nextValue
			if tc.mutate != nil {
				tc.mutate(previous, next)
			}
			oldPrevious, oldNext := clone(*previous), clone(*next)
			got := mergeSubscriptionObservation(previous, next, tc.errors)
			want := next
			if tc.keep {
				want = previous
			}
			if got != want {
				t.Fatalf("wrong complete observation selected: got=%+v want=%+v", got, want)
			}
			if !reflect.DeepEqual(*previous, oldPrevious) || !reflect.DeepEqual(*next, oldNext) {
				t.Fatal("merge mutated an observation or manufactured a mixed source")
			}
		})
	}
	if got := mergeSubscriptionObservation(nil, observationSubscriptionFixture(now), nil); got == nil {
		t.Fatal("first observation was discarded")
	}
	if got := mergeSubscriptionObservation(observationSubscriptionFixture(now), nil, nil); got != nil {
		t.Fatal("absent-candidate handling changed caller contract")
	}
}

func TestInsightSubscriptionObservationServicePreservesHistoryThenAcceptsNull(t *testing.T) {
	s, f := newFixture(t)
	clock := newReliabilityClock(s)
	original := observationSubscriptionFixture(clock.now())
	var rounds atomic.Int32
	f.query = func(context.Context) protocol.InsightResult {
		round := rounds.Add(1)
		next := observationSubscriptionFixture(clock.now())
		result := protocol.InsightResult{Subscription: next, AttemptedComponents: []string{"subscription"}}
		if round >= 2 {
			next.Source, next.ExpiresAt = "chatgpt_accounts", nil
			next.Reported = round >= 3
		}
		if round == 2 {
			result.Errors = []protocol.InsightError{{Component: "subscription", Code: "upstream_error", HTTPStatus: 503, Message: "订阅查询暂不可用"}}
		}
		return result
	}
	enable(t, s, false)
	reliabilityRefresh(t, s)
	first := waitResult(t, s, "success")
	if !reflect.DeepEqual(first.Subscription, original) {
		t.Fatal("initial subscription observation was not saved")
	}
	clock.advance(10 * time.Second)
	reliabilityRefresh(t, s)
	second := waitResult(t, s, "partial")
	if !reflect.DeepEqual(second.Subscription, first.Subscription) || len(second.Errors) != 1 || second.Errors[0].Component != "subscription" {
		t.Fatalf("partial metadata response replaced a trusted historical expiry: %+v", second)
	}
	clock.advance(2 * time.Minute)
	reliabilityRefresh(t, s)
	third := waitResult(t, s, "success")
	if third.Subscription == nil || !third.Subscription.Reported || third.Subscription.ExpiresAt != nil || third.Subscription.Source != "chatgpt_accounts" || !third.Subscription.ObservedAt.Equal(clock.now()) || len(third.Errors) != 0 {
		t.Fatalf("authoritative unknown failed to replace old expiry: %+v", third)
	}
}
