package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

type tokenOperation func(context.Context, Doer, string) (TokenSet, error)

func oauthOperations() map[string]tokenOperation {
	return map[string]tokenOperation{
		"exchange": func(ctx context.Context, d Doer, base string) (TokenSet, error) {
			return ExchangeOAuthCode(ctx, d, base, "fixture-client", "fixture-code", "fixture-verifier", DefaultCodexRedirectURI)
		},
		"refresh": func(ctx context.Context, d Doer, base string) (TokenSet, error) {
			return RefreshOAuthToken(ctx, d, base, "fixture-client", "fixture-refresh")
		},
	}
}

func TestOAuthExchangeAndRefreshShareChallengeSession(t *testing.T) {
	for name, operation := range oauthOperations() {
		t.Run(name, func(t *testing.T) {
			var ordinaryCalls, browserCalls, challengeCalls atomic.Int32
			var mu sync.Mutex
			var bodies, invocations []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
					t.Error("unexpected OAuth request")
					http.Error(w, "unexpected request", 500)
					return
				}
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(body))
				invocations = append(invocations, r.Header.Get("X-Access-Flow-Invocation-Id"))
				mu.Unlock()
				if cookie, err := r.Cookie("bound_session"); err != nil || cookie.Value != "fixture-session" {
					t.Error("OAuth session cookie changed")
				}
				if r.UserAgent() != "FixtureBrowser/1" {
					t.Error("OAuth challenge changed User-Agent")
				}
				if _, err := r.Cookie("verified"); err != nil {
					w.Header().Add("Set-Cookie", "challenge_session=fixture; Path=/")
					w.Header().Set("Cf-Mitigated", "challenge")
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, "browser verification required")
					return
				}
				if _, err := r.Cookie("challenge_session"); err != nil {
					t.Error("challenge Cookie was not preserved")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"access_token":"fixture-access","refresh_token":"fixture-refresh","id_token":"fixture-id"}`)
			}))
			defer server.Close()
			jar := transport.NewCookieJar()
			jar.SetFromResponse(server.URL, http.Header{"Set-Cookie": []string{"bound_session=fixture-session; Path=/"}})
			baseTransport := transport.NewClient(jar)
			client := NewClient(doerFunc(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
				ordinaryCalls.Add(1)
				return baseTransport.Do(ctx, r)
			}), jar, "auto")
			client.UserAgent = "FixtureBrowser/1"
			client.ConnectionRetries = 3
			client.Challenge = func(ctx context.Context, rawURL string) error {
				challengeCalls.Add(1)
				if rawURL != server.URL+"/oauth/token" || !strings.Contains(jar.HeaderFor(rawURL), "challenge_session=fixture") {
					t.Error("challenge lost original endpoint/session")
				}
				// Model the handoff hook to a real-browser transport while retaining the
				// session's jar and identity. Chromium itself has separate gated tests.
				jar.SetFromResponse(rawURL, http.Header{"Set-Cookie": []string{"verified=yes; Path=/"}})
				client.Transport = doerFunc(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
					browserCalls.Add(1)
					return baseTransport.Do(ctx, r)
				})
				return nil
			}
			tokens, err := operation(context.Background(), client, server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if tokens.AccessToken != "fixture-access" || ordinaryCalls.Load() != 1 || browserCalls.Load() != 1 || challengeCalls.Load() != 1 {
				t.Fatal("OAuth did not use exactly one session-preserving challenge handoff")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 2 || bodies[0] != bodies[1] || len(invocations) != 2 || invocations[0] == "" || invocations[0] != invocations[1] {
				t.Fatal("challenge changed token request body or invocation identity")
			}
		})
	}
}

func TestOAuthNonIdempotentConnectionFailureIsNotReplayed(t *testing.T) {
	for name, operation := range oauthOperations() {
		t.Run(name, func(t *testing.T) {
			calls, challenges := 0, 0
			connectionErr := errors.New("fixture connection lost after send")
			client := NewClient(doerFunc(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.Timeout != 37*time.Millisecond {
					t.Error("OAuth request lost method or configured timeout")
				}
				return nil, connectionErr
			}), nil, "auto")
			client.Timeout = 37 * time.Millisecond
			client.ConnectionRetries = 3
			client.Challenge = func(context.Context, string) error { challenges++; return nil }
			_, err := operation(context.Background(), client, "https://fixture.example")
			if !errors.Is(err, connectionErr) || calls != 1 || challenges != 0 {
				t.Fatalf("non-idempotent request replayed: calls=%d challenges=%d err=%v", calls, challenges, err)
			}
		})
	}
}

func TestOAuthOrdinaryRejectionsDoNotReplay(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		calls, challenges := 0, 0
		client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
			calls++
			return response(status, `{"error":"access_denied"}`), nil
		}), nil, "auto")
		client.ConnectionRetries = 3
		client.Challenge = func(context.Context, string) error { challenges++; return nil }
		if _, err := RefreshOAuthToken(context.Background(), client, "https://fixture.example", "client", "refresh"); err == nil || calls != 1 || challenges != 0 {
			t.Fatalf("ordinary HTTP %d rejection replayed", status)
		}
	}
}

func TestProtocolDoPreservesManualRedirectAndExplicitTimeout(t *testing.T) {
	calls := 0
	client := NewClient(doerFunc(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
		calls++
		if r.Timeout != 123*time.Millisecond || r.Method != http.MethodPost || string(r.Body) != "fixture-form" || r.Headers.Get("X-Access-Flow-Invocation-Id") != "fixture-invocation" {
			t.Error("adapter lost supplied request options")
		}
		return &transport.Response{StatusCode: 303, Header: http.Header{"Location": []string{"/next"}, "Set-Cookie": []string{"fixture=cookie"}}, RawSetCookie: []string{"fixture=cookie"}}, nil
	}), nil, "auto")
	response, err := client.Do(context.Background(), transport.Request{Method: http.MethodPost, URL: "https://fixture.example/token", Body: []byte("fixture-form"), Headers: http.Header{"X-Access-Flow-Invocation-Id": []string{"fixture-invocation"}}, Timeout: 123 * time.Millisecond})
	if err != nil || calls != 1 || response.StatusCode != 303 || response.Header.Get("Location") != "/next" || len(response.RawSetCookie) != 1 {
		t.Fatal("adapter followed redirect or lost response cookies")
	}
}

func TestFlowOAuthExchangeUsesClientChallengeHook(t *testing.T) {
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	calls, challenges := 0, 0
	jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"fixture@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`)) + ".x"
	client := NewClient(doerFunc(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
		calls++
		if calls == 1 {
			return response(403, "/cdn-cgi/challenge-platform/"), nil
		}
		body, _ := json.Marshal(TokenSet{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: jwt})
		return response(200, string(body)), nil
	}), nil, "auto")
	client.Challenge = func(context.Context, string) error { challenges++; return nil }
	flow := NewFlow(client, FlowOptions{Email: "fixture@example.test", AuthBase: "https://auth.example", OutputMode: "sub2api"})
	checkpoint := Checkpoint{Version: 1, Stage: "callback_ready", Email: "fixture@example.test", Cookies: []transport.Cookie{}, OAuth: map[string]any{"state": pkce.State, "codeVerifier": pkce.CodeVerifier, "callbackUrl": DefaultCodexRedirectURI + "?" + url.Values{"state": []string{pkce.State}, "code": []string{"fixture-code"}}.Encode()}}
	if err = flow.Restore(checkpoint); err != nil {
		t.Fatal(err)
	}
	result, err := flow.Run(context.Background())
	if err != nil || len(result.Export.Accounts) != 1 || calls != 2 || challenges != 1 {
		t.Fatalf("Flow exchange bypassed challenge hook: calls=%d challenges=%d err=%v", calls, challenges, err)
	}
}

func TestOAuthFailedChallengeDoesNotReplayTokenRequest(t *testing.T) {
	calls := 0
	challengeErr := errors.New("fixture browser verification canceled")
	client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		return response(503, "/cdn-cgi/challenge-platform/"), nil
	}), nil, "auto")
	client.Challenge = func(context.Context, string) error { return challengeErr }
	_, err := ExchangeOAuthCode(context.Background(), client, "https://fixture.example", "client", "code", "verifier", DefaultCodexRedirectURI)
	if !errors.Is(err, challengeErr) || calls != 1 {
		t.Fatalf("token request retried without completed browser verification: calls=%d err=%v", calls, err)
	}
}
