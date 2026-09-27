package protocol

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestBrowserChallengeRequiresExplicitEvidence(t *testing.T) {
	tests := []struct {
		name, contentType, body, header string
		status                          int
		want                            bool
	}{
		{name: "ordinary-forbidden-html", status: 403, contentType: "text/html", body: "<html><title>Access denied</title>Permission denied</html>"},
		{name: "ordinary-forbidden-cf-assets", status: 403, contentType: "text/html", body: "<html><link href='/cdn-cgi/styles/main.css'><title>Access denied</title></html>"},
		{name: "ordinary-forbidden-wait-message", status: 403, contentType: "text/html", body: "<html><p>Please try again in just a moment.</p></html>"},
		{name: "ordinary-rate-limit-json", status: 429, contentType: "application/json", body: `{"error":"rate_limit_exceeded"}`},
		{name: "ordinary-rate-limit-html", status: 429, contentType: "text/html", body: "<html><title>Too many requests</title></html>"},
		{name: "ordinary-provider-json-marker", status: 403, contentType: "application/json", body: `{"error":"account_disabled","message":"See /cdn-cgi/challenge-platform/ documentation"}`},
		{name: "ordinary-problem-json-marker", status: 403, contentType: "application/problem+json", body: `{"detail":"cf-chl-error"}`},
		{name: "ordinary-json-without-type", status: 403, body: `{"error":"just a moment cf-challenge"}`},
		{name: "ordinary-json-mislabeled-html", status: 403, contentType: "text/html", body: `{"error":"/cdn-cgi/challenge-platform/"}`},
		{name: "ordinary-unavailable-cf-assets", status: 503, contentType: "text/html", body: "<html><link href='/cdn-cgi/styles/main.css'>Service unavailable</html>"},
		{name: "nonchallenge-header-value", status: 403, header: "not-challenge", body: "Forbidden"},
		{name: "mitigated-forbidden", status: 403, header: "challenge", body: "Provider verification required", want: true},
		{name: "mitigated-rate-limit", status: 429, header: " Challenge ", body: "Provider verification required", want: true},
		{name: "mitigated-overrides-json", status: 403, header: "challenge", contentType: "application/json", body: `{"error":"blocked"}`, want: true},
		{name: "challenge-title", status: 403, contentType: "text/html", body: "<html><title>Just a moment...</title></html>", want: true},
		{name: "challenge-script-fragment", status: 403, body: "<script src='/cdn-cgi/challenge-platform/test.js'></script>", want: true},
		{name: "challenge-legacy-fragment", status: 403, body: "/cdn-cgi/challenge-platform/", want: true},
		{name: "challenge-runtime-marker", status: 403, body: "<script>window._cf_chl_opt={};</script>", want: true},
		{name: "challenge-unavailable", status: 503, body: "/cdn-cgi/challenge-platform/", want: true},
		{name: "challenge-bad-request", status: 400, contentType: "text/html", body: "<html><script src='/cdn-cgi/challenge-platform/test.js'></script></html>", want: true},
		{name: "challenge-conflict", status: 409, contentType: "text/html", body: "<html><title>Just a moment...</title></html>", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := response(test.status, test.body)
			r.Header.Set("Content-Type", test.contentType)
			r.Header.Set("Cf-Mitigated", test.header)
			if got := isBrowserChallenge(r); got != test.want {
				t.Fatalf("challenge=%v want=%v", got, test.want)
			}
		})
	}
}

func TestClientOrdinaryHTTPRejectionsDoNotInvokeBrowser(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			calls, challenges := 0, 0
			r := response(status, "<html><link href='/cdn-cgi/styles/main.css'><title>Access denied</title></html>")
			r.Header.Set("Content-Type", "text/html")
			client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return r, nil
			}), nil, "auto")
			client.ConnectionRetries = 3
			client.Challenge = func(context.Context, string) error { challenges++; return nil }
			got, err := client.Request(context.Background(), method, "https://fixture.example/api/accounts/operation", RequestOptions{})
			if err != nil || got != r || got.StatusCode != status || calls != 1 || challenges != 0 {
				t.Fatalf("ordinary rejection triggered browser/replay: status=%d method=%s calls=%d challenges=%d err=%v", status, method, calls, challenges, err)
			}
			_, err = client.JSON(context.Background(), method, "https://fixture.example/api/accounts/operation", nil, RequestOptions{}, nil)
			var rejected *HTTPError
			if !errors.As(err, &rejected) || rejected.StatusCode != status || calls != 2 || challenges != 0 {
				t.Fatalf("ordinary rejection treated as successful JSON: status=%d err=%v", status, err)
			}
		}
	}
}

func TestClientChallengeReplayRetainsBrowserAndRejectsOrdinaryDenial(t *testing.T) {
	goCalls, browserCalls, challenges := 0, 0, 0
	var invocation string
	client := NewClient(doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		goCalls++
		invocation = r.Headers.Get("X-Access-Flow-Invocation-Id")
		result := response(403, "Provider verification required")
		result.Header.Set("Cf-Mitigated", "challenge")
		return result, nil
	}), nil, "auto")
	bound := &struct{ Doer }{doerFunc(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		browserCalls++
		if r.Headers.Get("User-Agent") != "fixture-browser-identity" || invocation == "" || r.Headers.Get("X-Access-Flow-Invocation-Id") != invocation {
			t.Error("challenge replay lost bound identity or logical invocation")
		}
		result := response(403, "<html><title>Access denied</title><link href='/cdn-cgi/styles/main.css'></html>")
		result.Header.Set("Content-Type", "text/html")
		return result, nil
	})}
	client.Challenge = func(context.Context, string) error {
		challenges++
		client.Transport = bound
		client.UserAgent = "fixture-browser-identity"
		return nil
	}
	_, err := client.JSON(context.Background(), http.MethodPost, "https://fixture.example/api/accounts/operation", map[string]string{"action": "fixture"}, RequestOptions{}, nil)
	var rejected *HTTPError
	if !errors.As(err, &rejected) || rejected.StatusCode != 403 || errors.Is(err, ErrBrowserChallenge) {
		t.Fatalf("challenge replay ordinary denial was not returned as HTTP error: %v", err)
	}
	if goCalls != 1 || browserCalls != 1 || challenges != 1 || client.Transport != bound {
		t.Fatalf("browser binding changed or extra replay: go=%d browser=%d challenge=%d", goCalls, browserCalls, challenges)
	}
}
