package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

type insightFixtureDoer func(context.Context, transport.Request) (*transport.Response, error)

func (f insightFixtureDoer) Do(ctx context.Context, r transport.Request) (*transport.Response, error) {
	return f(ctx, r)
}

func insightTestCredentials() InsightCredentials {
	return InsightCredentials{AccessToken: "synthetic-access-secret", AccountID: "workspace", Email: "owner@example.test", AuthorizationMethod: "chatgpt", PlanType: "plus"}
}
func insightResponse(status int, body string) *transport.Response {
	return &transport.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}
func insightJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func insightAccountsFixture(expiry any) string {
	return string(insightJSON(map[string]any{"accounts": map[string]any{"workspace": map[string]any{"account": map[string]any{"account_id": "workspace", "plan_type": "plus"}, "entitlement": map[string]any{"expires_at": expiry}}}}))
}

func TestInsightsOnlyFixedReadsAndIdentityHeaders(t *testing.T) {
	for _, method := range []string{"chatgpt", "bps"} {
		t.Run(method, func(t *testing.T) {
			creds := insightTestCredentials()
			creds.AuthorizationMethod = method
			calls := map[string]int{}
			client := &InsightsClient{Transport: insightFixtureDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
				calls[r.URL]++
				if r.Method != http.MethodGet || len(r.Body) != 0 || !r.SingleAttempt || r.RetryReadOnly || r.RetryBeforeSend || r.MaxResponseBytes != insightMaxBodyBytes {
					t.Fatal("unsafe query request")
				}
				if !insightAllowedURL(r.URL) || r.Headers.Get("Authorization") != "Bearer "+creds.AccessToken || r.Headers.Get("Chatgpt-Account-Id") != creds.AccountID || r.Headers.Get("Oai-Timezone") != "America/Los_Angeles" {
					t.Fatal("query lost endpoint or identity binding")
				}
				if r.Headers.Get("Cookie") != "" || r.Headers.Get("Openai-Sentinel-Token") != "" {
					t.Fatal("query borrowed authentication session state")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 30*time.Second {
					t.Fatal("query has no bounded context")
				}
				switch r.URL {
				case insightUsageURL:
					return insightResponse(200, "{\"account_id\":\"workspace\",\"email\":\"owner@example.test\",\"rate_limit\":{\"allowed\":true,\"limit_reached\":false,\"primary_window\":{\"used_percent\":0,\"limit_window_seconds\":18000,\"reset_after_seconds\":0}},\"credits\":{\"has_credits\":true,\"unlimited\":false,\"balance\":\"0.000000000000000001\"}}"), nil
				case insightBPSAccessURL:
					if r.Headers.Get("X-Openai-Account-Id") != creds.AccountID || r.Headers.Get("X-Basispoints-Auth-Mode") != "chatgpt" || r.Headers.Get("Openai-Beta") != "" {
						t.Fatal("BPS headers mixed authorization routes")
					}
					return insightResponse(200, "{\"allowed\":true,\"model_catalog\":{\"models\":[]}}"), nil
				case insightAccountsURL:
					return insightResponse(200, insightAccountsFixture("2030-01-01T00:00:00Z")), nil
				case insightResetCreditsURL:
					return insightResponse(200, "{\"available_count\":0,\"credits\":[]}"), nil
				default:
					t.Fatal("unexpected endpoint")
					return nil, nil
				}
			})}
			result := client.Query(context.Background(), creds, InsightsOptions{IncludeResetCredits: true})
			if len(result.Errors) != 0 || result.Usage == nil || result.Subscription == nil || !result.Subscription.IdentityVerified {
				t.Fatalf("query did not produce verified results: %+v", result.Errors)
			}
			if (result.BPS != nil) != (method == "bps") {
				t.Fatal("BPS query did not follow saved authorization method")
			}
			if result.Usage.Windows[0].UsedPercent == nil || *result.Usage.Windows[0].UsedPercent != 0 || result.Usage.ResetCredits.AvailableCount == nil || *result.Usage.ResetCredits.AvailableCount != 0 {
				t.Fatal("valid zero was discarded")
			}
			if *result.Usage.Credits.Balance != "0.000000000000000001" {
				t.Fatal("credit decimal lost precision")
			}
			for _, n := range calls {
				if n != 1 {
					t.Fatal("successful query repeated")
				}
			}
			data, _ := json.Marshal(result)
			if strings.Contains(string(data), creds.AccessToken) {
				t.Fatal("result leaked access token")
			}
		})
	}
}

func TestInsightsBPSPartialSuccessDoesNotRefreshOrProbe(t *testing.T) {
	creds := insightTestCredentials()
	creds.AuthorizationMethod = "bps"
	client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		if r.Method != "GET" || !insightAllowedURL(r.URL) {
			t.Fatal("query attempted side effect")
		}
		if r.URL == insightBPSAccessURL {
			return insightResponse(200, "{\"allowed\":false}"), nil
		}
		return insightResponse(403, "{\"error\":{\"message\":\"synthetic-access-secret cookie=secret\"}}"), nil
	})}
	r := client.Query(context.Background(), creds, InsightsOptions{})
	if r.Usage != nil || r.BPS == nil || r.BPS.Allowed == nil || *r.BPS.Allowed || len(r.Errors) != 3 {
		t.Fatalf("partial results collapsed: %+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "secret") {
		t.Fatal("upstream rejection body leaked")
	}
}

func TestInsightUsageNullUnknownAndMalformedValues(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for _, body := range []string{"{\"rate_limit\":null,\"credits\":null}", "{\"rate_limit\":{\"primary_window\":{\"used_percent\":null,\"limit_window_seconds\":null}},\"credits\":{\"has_credits\":null,\"unlimited\":null,\"balance\":null}}"} {
		u, err := parseInsightUsage([]byte(body), insightTestCredentials(), now)
		if err != nil || u.Allowed != nil || u.LimitReached != nil {
			t.Fatalf("unknown became false: %v", err)
		}
		if len(u.Windows) > 0 && (u.Windows[0].UsedPercent != nil || u.Windows[0].WindowSeconds != nil || u.Windows[0].ResetsAt != nil) {
			t.Fatal("missing window fields became zero values")
		}
	}
	for _, body := range []string{"{}", "null", "[]", "{\"rate_limit\":true}", "{\"rate_limit\":{\"allowed\":0}}", "{\"rate_limit\":{\"primary_window\":{\"limit_window_seconds\":-1}}}", "{\"credits\":{\"balance\":12}}", "{\"credits\":{\"balance\":\"not-decimal\"}}", "{\"credits\":null} {}", "{\"rate_limit\":{\"primary_window\":{\"reset_after_seconds\":9223372036854775807}}}"} {
		if _, err := parseInsightUsage([]byte(body), insightTestCredentials(), now); err == nil {
			t.Fatalf("malformed response accepted: %s", body)
		}
	}
	for _, body := range []string{"{\"account_id\":\"other\",\"credits\":null}", "{\"account_id\":\"workspace\",\"chatgpt_account_id\":\"other\",\"credits\":null}", "{\"email\":\"other@example.test\",\"credits\":null}"} {
		if _, err := parseInsightUsage([]byte(body), insightTestCredentials(), now); !errors.Is(err, errInsightIdentity) {
			t.Fatal("foreign identity accepted")
		}
	}
}

func TestInsightUsageFeaturesDoNotOverwriteMainQuota(t *testing.T) {
	body := "{\"rate_limit\":{\"allowed\":true,\"primary_window\":{\"used_percent\":5,\"limit_window_seconds\":604800}},\"additional_rate_limits\":[{\"limit_name\":\"Spark\",\"metered_feature\":\"codex_bengalfox\",\"rate_limit\":{\"allowed\":false,\"limit_reached\":true,\"primary_window\":{\"used_percent\":120,\"limit_window_seconds\":18000}}}]}"
	u, err := parseInsightUsage([]byte(body), insightTestCredentials(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if *u.Windows[0].UsedPercent != 5 || *u.Features[0].Windows[0].UsedPercent != 120 || *u.Features[0].Allowed || !*u.Allowed {
		t.Fatal("feature quota contaminated main quota or raw percentage was clamped")
	}
}

func TestInsightGetBlocksRedirectsMalformedBodiesAndUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{"http://chatgpt.com/backend-api/wham/usage", insightUsageURL + "?x=1", "https://chatgpt.com:443/backend-api/wham/usage", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", "https://evil.example/backend-api/wham/usage", "https://user@chatgpt.com/backend-api/wham/usage", insightSubscriptionsURL + "?account_id=x&account_id=y", insightSubscriptionsURL + "?account_id=x&redirect=https://evil.example"} {
		if insightAllowedURL(endpoint) {
			t.Fatalf("untrusted endpoint accepted: %s", endpoint)
		}
	}
	for _, tc := range []struct {
		name     string
		response *transport.Response
		err      error
		code     string
	}{
		{"redirect", &transport.Response{StatusCode: 302, Header: http.Header{"Location": {"https://evil.example"}}}, nil, "redirect_blocked"},
		{"html", insightResponse(200, "<html>synthetic secret</html>"), nil, "challenge_or_html"},
		{"oversized-error", insightResponse(403, strings.Repeat("s", insightMaxBodyBytes+1)), nil, "response_too_large"},
		{"transport-limit", nil, transport.ErrResponseTooLarge, "response_too_large"},
		{"private-error", nil, errors.New("synthetic-access-secret"), "network_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return tc.response, tc.err
			})}
			_, e := c.get(context.Background(), "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, time.Second)
			if e == nil || e.Code != tc.code || calls != 1 || strings.Contains(e.Message, "secret") {
				t.Fatalf("unsafe failure handling: %+v calls=%d", e, calls)
			}
		})
	}
	calls := 0
	c := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, nil })}
	_, e := c.get(context.Background(), "usage", "https://evil.example/", insightTestCredentials(), InsightsOptions{}, time.Second)
	if e.Code != "endpoint_rejected" || calls != 0 {
		t.Fatal("endpoint validation happened after dispatch")
	}
	u, _ := url.Parse("https://evil.example/escaped")
	r := insightResponse(200, "{}")
	r.URL = u
	c.Transport = insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) { return r, nil })
	_, e = c.get(context.Background(), "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, time.Second)
	if e.Code != "redirect_blocked" {
		t.Fatal("unexpected final response origin accepted")
	}
}

func TestInsightGetRetriesOnlyTransientReadsWithinSharedBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		err    error
		want   int
	}{
		{"temporary", 0, io.ErrUnexpectedEOF, 2}, {"rejected-truncated", 403, io.ErrUnexpectedEOF, 1}, {"business-rejection", 429, nil, 1}, {"upstream-503", 503, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				if calls == 1 {
					if tc.status == 0 {
						return nil, tc.err
					}
					return insightResponse(tc.status, "{}"), tc.err
				}
				return insightResponse(200, "{}"), nil
			})}
			_, _ = c.get(context.Background(), "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, time.Second)
			if calls != tc.want {
				t.Fatalf("calls=%d want=%d", calls, tc.want)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	calls := 0
	c := &InsightsClient{Transport: insightFixtureDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
		calls++
		<-ctx.Done()
		return nil, ctx.Err()
	})}
	r := c.Query(ctx, insightTestCredentials(), InsightsOptions{IncludeResetCredits: true})
	if calls != 1 || len(r.Errors) == 0 || r.Errors[0].Code != "timeout" {
		t.Fatal("deadline restarted for later endpoints")
	}
}

func TestInsightErrorsPreserveRetryAndUnsupportedDistinctions(t *testing.T) {
	r := insightResponse(429, "{\"error\":\"private\"}")
	r.Header.Set("Retry-After", "120")
	e := insightHTTPFailure("usage", r)
	if e.Code != "rate_limited" || e.RetryAfter == nil || time.Until(*e.RetryAfter) > 121*time.Second || e.HTTPStatus != 429 {
		t.Fatal("retry-after lost")
	}
	for _, tc := range []struct{ body, code string }{{"{\"error\":{\"code\":\"insufficient_scope\"}}", "unsupported_authorization"}, {"<html>challenge</html>", "permission_denied"}, {"{\"error\":{\"message\":\"insufficient_scope\"}}", "permission_denied"}} {
		if got := insightHTTPFailure("usage", insightResponse(403, tc.body)); got.Code != tc.code {
			t.Fatalf("ambiguous rejection became unsupported: %v", got)
		}
	}
}

func TestInsightsRejectAuthenticationWrapperAndInvalidInputs(t *testing.T) {
	creds := insightTestCredentials()
	calls := 0
	doer := insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, nil })
	c := &InsightsClient{Transport: &Client{Transport: doer, Challenge: func(context.Context, string) error { t.Fatal("challenge invoked"); return nil }}}
	if result := c.Query(context.Background(), creds, InsightsOptions{}); len(result.Errors) != 1 || result.Errors[0].Code != "unsafe_transport" {
		t.Fatal("authentication wrapper accepted")
	}
	c.Transport = doer
	creds.AccessToken = "secret\r\nX-Evil: header"
	if result := c.Query(context.Background(), creds, InsightsOptions{}); result.Errors[0].Code != "invalid_credentials" {
		t.Fatal("header injection accepted")
	}
	if result := c.Query(context.Background(), insightTestCredentials(), InsightsOptions{Timezone: "invalid/location"}); result.Errors[0].Code != "invalid_timezone" {
		t.Fatal("invalid timezone accepted")
	}
	if calls != 0 {
		t.Fatal("invalid inputs dispatched requests")
	}
	b, _ := json.Marshal(insightTestCredentials())
	if string(b) != "{}" {
		t.Fatal("credentials are serializable")
	}
}

func FuzzAccountInsightsParsers(f *testing.F) {
	for _, seed := range []string{"{}", "null", "{\"rate_limit\":{\"allowed\":true}}", "{\"allowed\":false}", "{\"available_count\":0}", insightAccountsFixture(nil)} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > insightMaxBodyBytes {
			return
		}
		now := time.Unix(1800000000, 0)
		creds := insightTestCredentials()
		_, _ = parseInsightUsage(b, creds, now)
		_, _ = parseInsightBPS(b, creds, now)
		_, _ = parseInsightResetDetails(b, now)
		_, _ = parseInsightAccounts(b, creds, now)
		_, _ = parseInsightSubscription(b, creds, false, now)
	})
}
