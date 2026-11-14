package protocol

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestInsightDiagnosticsRetainReceivedStatusAfterBodyFailures(t *testing.T) {
	for _, scenario := range []struct {
		name, code                 string
		status, calls              int
		body                       string
		err                        error
		redirect, cancel, deadline bool
	}{
		{name: "redirect-url", code: "redirect_blocked", status: 302, calls: 1, redirect: true},
		{name: "redirect-final-200", code: "redirect_blocked", status: 200, calls: 1, redirect: true},
		{name: "html", code: "challenge_or_html", status: 200, calls: 1, body: "<html>synthetic-secret</html>"},
		{name: "read-error", code: "network_error", status: 200, calls: 1, err: errors.New("synthetic-secret")},
		{name: "retryable-body-error", code: "network_error", status: 200, calls: 2, err: io.ErrUnexpectedEOF},
		{name: "cancel-with-response-error", code: "canceled", status: 200, calls: 1, err: context.Canceled, cancel: true},
		{name: "cancel-with-response", code: "canceled", status: 200, calls: 1, cancel: true},
		{name: "deadline-with-response", code: "timeout", status: 200, calls: 1, err: context.DeadlineExceeded, deadline: true},
		{name: "no-response", code: "network_error", status: 0, calls: 1, err: errors.New("synthetic-secret HTTP 403")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			client := &InsightsClient{Transport: insightFixtureDoer(func(requestCtx context.Context, request transport.Request) (*transport.Response, error) {
				calls++
				if scenario.cancel {
					cancel()
				}
				if scenario.deadline {
					<-requestCtx.Done()
				}
				if scenario.status == 0 {
					return nil, scenario.err
				}
				response := insightResponse(scenario.status, scenario.body)
				if scenario.redirect {
					response.URL, _ = url.Parse("https://private.example/?synthetic-secret")
				}
				return response, scenario.err
			})}
			budget := time.Second
			if scenario.deadline {
				budget = 5 * time.Millisecond
			}
			_, failure := client.get(ctx, "usage", insightUsageURL, insightTestCredentials(), InsightsOptions{}, budget)
			if failure == nil || failure.Code != scenario.code || failure.HTTPStatus != scenario.status || calls != scenario.calls {
				t.Fatalf("response diagnostics changed status, category or retries: failure=%+v calls=%d", failure, calls)
			}
			assertInsightDiagnosticPrivate(t, failure)
		})
	}
}

func TestInsightDiagnosticsParserFailuresRetainHTTP200(t *testing.T) {
	for _, component := range []string{"usage", "bps", "resetCredits", "subscription"} {
		t.Run(component, func(t *testing.T) {
			creds := insightTestCredentials()
			creds.AuthorizationMethod = AuthorizationMethodBPS
			options := InsightsOptions{IncludeResetCredits: component == "resetCredits"}
			for _, candidate := range []string{"usage", "bps", "resetCredits", "subscription"} {
				if candidate != component && !(component == "resetCredits" && candidate == "usage") {
					options.SkipComponents = append(options.SkipComponents, candidate)
				}
			}
			client := &InsightsClient{Transport: insightFixtureDoer(func(_ context.Context, request transport.Request) (*transport.Response, error) {
				if component == "resetCredits" && request.URL == insightUsageURL {
					return insightResponse(http.StatusOK, `{"rate_limit":null}`), nil
				}
				return insightResponse(http.StatusOK, "synthetic-secret malformed JSON"), nil
			})}
			result := client.Query(t.Context(), creds, options)
			if len(result.Errors) == 0 {
				t.Fatal("malformed response produced no diagnostic")
			}
			for _, failure := range result.Errors {
				if failure.Component != component || failure.Code != "protocol_error" || failure.HTTPStatus != http.StatusOK {
					t.Fatalf("parser response lost actual status: %+v", failure)
				}
				assertInsightDiagnosticPrivate(t, failure)
			}
		})
	}
}

func TestInsightDiagnosticsIdentityFailureRetainsHTTP200(t *testing.T) {
	client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return insightResponse(http.StatusOK, `{"account_id":"different-workspace","rate_limit":null}`), nil
	})}
	result := client.Query(t.Context(), insightTestCredentials(), InsightsOptions{SkipComponents: []string{"bps", "subscription", "resetCredits"}})
	if len(result.Errors) != 1 || result.Errors[0].Code != "identity_mismatch" || result.Errors[0].HTTPStatus != http.StatusOK {
		t.Fatalf("identity error lost response status: %+v", result.Errors)
	}
}
