package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestInsightResetCreditsUnknownZeroAliasesAndPrivateIDs(t *testing.T) {
	now := time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC)
	for _, body := range []string{`{"available_count":null}`, `{"credits":null}`} {
		got, err := parseInsightResetDetails([]byte(body), now)
		if err != nil || got.AvailableCount != nil || got.Credits != nil {
			t.Fatalf("unknown credits became zero: %+v %v", got, err)
		}
	}
	for _, body := range []string{`{"availableCount":"0"}`, `{"available_count":0,"availableCount":"0"}`, `[]`, `{"items":[]}`} {
		got, err := parseInsightResetDetails([]byte(body), now)
		if err != nil || got.AvailableCount == nil || *got.AvailableCount != 0 {
			t.Fatalf("valid zero credits lost: %+v %v", got, err)
		}
	}
	body := `{"data":[
		{"id":"synthetic-consume-id","reset_type":"codex_rate_limits","status":"available","expires_at":"2030-01-01T08:00:00+08:00"},
		{"creditId":"synthetic-consume-id-2","resetType":"codex_rate_limits","expiresAt":null},
		{"id":"synthetic-used-id","status":"used","expires_at":"2030-01-01T00:00:00Z"},
		{"id":"synthetic-other-pool-id","reset_type":"another_pool"},
		null
	]}`
	got, err := parseInsightResetDetails([]byte(body), now)
	if err != nil || got.AvailableCount == nil || *got.AvailableCount != 2 || len(got.Credits) != 2 || got.Credits[0].ExpiresAt == nil || got.Credits[0].ExpiresAt.Format(time.RFC3339) != "2030-01-01T00:00:00Z" || got.Credits[1].ExpiresAt != nil {
		t.Fatalf("reset detail semantics changed: %+v %v", got, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "synthetic-") || strings.Contains(string(encoded), "creditId") {
		t.Fatal("consume identifiers escaped the private parser")
	}
	for _, body := range []string{
		`{}`, `{"unrecognized":0}`, `null`, `{"available_count":-1}`, `{"available_count":1.5}`,
		`{"available_count":1,"availableCount":2}`, `{"credits":[],"items":[]}`,
		`{"credits":[{"reset_type":"codex_rate_limits","resetType":"other"}]}`,
		`{"credits":[{"expires_at":"2030-01-01T00:00:00Z","expiresAt":"2031-01-01T00:00:00Z"}]}`,
		`{"credits":[{"status":1}]}`, `{"credits":[{"expires_at":"invalid"}]}`,
	} {
		if _, err := parseInsightResetDetails([]byte(body), now); err == nil {
			t.Fatalf("malformed reset credits accepted: %s", body)
		}
	}
}

func TestInsightJSONRejectsDuplicateKeysAndExcessiveNesting(t *testing.T) {
	for _, body := range []string{
		`{"account_id":"other","account_id":"workspace","rate_limit":null}`,
		`{"account_id":"other","account_\u0069d":"workspace","rate_limit":null}`,
		`{"rate_limit":{"allowed":false,"allowed":true}}`,
		`{"rate_limit":null,"extension":[{"email":"other@example.test","email":"owner@example.test"}]}`,
		`{"rate_limit":null} {"rate_limit":null}`,
		`{"extension":` + strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66) + "}",
	} {
		if _, err := insightObject([]byte(body)); !errors.Is(err, errInsightProtocol) {
			t.Fatal("ambiguous or unbounded JSON accepted")
		}
	}
	if _, err := insightObject([]byte(`{"rate_limit":null,"extension":{"future_number":1e999}}`)); err != nil {
		t.Fatal("unknown numeric extension was unnecessarily interpreted")
	}
	if _, err := parseInsightResetDetails([]byte(`[{"status":"used","status":"available"}]`), time.Now()); err == nil {
		t.Fatal("root-array duplicate member escaped validation")
	}
}

func TestInsightsOptionalResetFailureKeepsCoreUsage(t *testing.T) {
	for _, usageStatus := range []int{200, 403} {
		resetCalls := 0
		client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
			switch request.URL {
			case insightUsageURL:
				return insightResponse(usageStatus, `{"credits":{"balance":"12.345"}}`), nil
			case insightAccountsURL:
				return insightResponse(200, insightAccountsFixture("2030-01-01T00:00:00Z")), nil
			case insightResetCreditsURL:
				resetCalls++
				return insightResponse(503, `{"error":"temporary"}`), nil
			default:
				t.Fatalf("unexpected optional endpoint: %s", request.URL)
				return nil, nil
			}
		})}
		result := client.Query(context.Background(), insightTestCredentials(), InsightsOptions{IncludeResetCredits: true})
		if usageStatus == 200 {
			if resetCalls != 1 || result.Usage == nil || result.Usage.Credits == nil || *result.Usage.Credits.Balance != "12.345" || result.Usage.ResetCredits != nil || len(result.Errors) != 1 || result.Errors[0].Component != "resetCredits" {
				t.Fatalf("optional failure erased core usage: %+v", result)
			}
		} else if resetCalls != 0 || result.Usage != nil || result.Subscription == nil {
			t.Fatal("reset detail request ignored core rejection or erased subscription")
		}
	}
}

func TestInsightRetryCancellationAndBackoffDoNotRedispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		cancel()
		return nil, io.ErrUnexpectedEOF
	})}
	_, failure := client.get(ctx, "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, time.Second)
	if calls != 1 || failure == nil || failure.Code != "canceled" {
		t.Fatalf("canceled query retried: calls=%d failure=%+v", calls, failure)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, value := range []string{"-1", "invalid", now.Add(-time.Hour).Format(http.TimeFormat)} {
		if got := insightRetryAfter(value, now); got != nil {
			t.Fatalf("invalid cooldown accepted: %v", value)
		}
	}
	if got := insightRetryAfter(now.Add(72*time.Hour).Format(http.TimeFormat), now); got == nil || !got.Equal(now.Add(24*time.Hour)) {
		t.Fatal("unbounded upstream cooldown accepted")
	}
}

func TestInsightTruncatedRejectionKeepsHTTPStatusAndCooldown(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		calls := 0
		client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
			calls++
			response := insightResponse(status, "{partial")
			response.Header.Set("Retry-After", "120")
			return response, io.ErrUnexpectedEOF
		})}
		_, failure := client.get(context.Background(), "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, time.Second)
		if failure == nil || failure.HTTPStatus != status || failure.Code == "network_error" || calls != 1 {
			t.Fatalf("known rejection became a retryable transport failure: status=%d failure=%+v calls=%d", status, failure, calls)
		}
		if (failure.RetryAfter != nil) != (status == 429 || status == 503) {
			t.Fatalf("HTTP cooldown changed after body interruption: %+v", failure)
		}
	}
}
