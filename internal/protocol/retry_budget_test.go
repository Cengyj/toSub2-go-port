package protocol

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestConfiguredReadRetryBudgetAndZeroDelay(t *testing.T) {
	for _, retries := range []int{0, 4, 20} {
		calls := 0
		failure := io.EOF
		client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, failure }), nil, "")
		if client.RetryDelay != 200*time.Millisecond {
			t.Fatal("constructor lost sensible default delay")
		}
		client.ConnectionRetries, client.RetryDelay = retries, 0
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := client.Request(ctx, http.MethodGet, "https://fixture.test", RequestOptions{})
		cancel()
		if !errors.Is(err, failure) || calls != retries+1 {
			t.Fatalf("retry budget silently reduced or zero delay ignored: retries=%d calls=%d err=%v", retries, calls, err)
		}
	}
}
func TestConfiguredReadRetryCancellationAndNoWriteReplay(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		calls := 0
		failure := io.ErrUnexpectedEOF
		client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, failure }), nil, "")
		client.ConnectionRetries, client.RetryDelay = 20, time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := client.Request(ctx, method, "https://fixture.test/oauth/token", RequestOptions{})
		cancel()
		if calls != 1 {
			t.Fatal("cancellation or non-idempotent operation allowed extra request", method, calls)
		}
		if method == http.MethodGet || method == http.MethodHead {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("retry wait ignored deadline", err)
			}
		} else if !errors.Is(err, failure) {
			t.Fatal("write failure lost", err)
		}
	}
}
func TestInvalidReadRetryBudgetFailsExplicitly(t *testing.T) {
	for _, retries := range []int{-1, 21} {
		calls := 0
		client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
			calls++
			return response(200, "{}"), nil
		}), nil, "")
		client.ConnectionRetries = retries
		if _, err := client.Request(context.Background(), http.MethodGet, "https://fixture.test", RequestOptions{}); err == nil || calls != 0 {
			t.Fatal("invalid budget silently clamped")
		}
	}
}

func TestReadTimeoutRetriesRecoverWithoutPrematureFailure(t *testing.T) {
	calls := 0
	var events []transport.ReadRetryEvent
	client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		if calls < 3 {
			return nil, context.DeadlineExceeded
		}
		return response(200, "{}"), nil
	}), nil, "")
	client.ConnectionRetries, client.RetryDelay = 3, 0
	client.OnRetry = func(e transport.ReadRetryEvent) { events = append(events, e) }
	got, err := client.Request(t.Context(), http.MethodGet, "https://fixture.test", RequestOptions{})
	if err != nil || got.StatusCode != 200 || calls != 3 || len(events) != 2 {
		t.Fatalf("transient timeout stopped a safe read: calls=%d events=%v err=%v", calls, events, err)
	}
	for i, event := range events {
		if event.Retry != i+1 || event.MaxRetries != 3 || event.Exhausted {
			t.Fatalf("recovered read reported an exhausted or incorrect retry: %+v", event)
		}
	}
}

func TestSafeReadAfterChallengeAndSecurityReadRetryBoundaries(t *testing.T) {
	t.Run("challenge GET retains retry budget", func(t *testing.T) {
		calls, challenges := 0, 0
		client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
			calls++
			if calls == 1 {
				r := response(403, "Provider verification required")
				r.Header.Set("Cf-Mitigated", "challenge")
				return r, nil
			}
			if calls < 4 {
				return nil, context.DeadlineExceeded
			}
			return response(200, "{}"), nil
		}), nil, "")
		client.ConnectionRetries, client.RetryDelay = 3, 0
		client.Challenge = func(context.Context, string) error { challenges++; return nil }
		if _, err := client.Request(t.Context(), http.MethodGet, "https://fixture.test", RequestOptions{}); err != nil || calls != 4 || challenges != 1 {
			t.Fatalf("challenge read bypassed retries: calls=%d challenges=%d err=%v", calls, challenges, err)
		}
	})
	t.Run("security read forbids challenge replay", func(t *testing.T) {
		calls := 0
		client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
			calls++
			r := response(403, "Provider verification required")
			r.Header.Set("Cf-Mitigated", "challenge")
			return r, nil
		}), nil, "")
		client.ConnectionRetries, client.RetryDelay = 3, 0
		client.Challenge = func(context.Context, string) error { t.Fatal("security read replayed challenge"); return nil }
		if _, err := client.Request(t.Context(), http.MethodGet, "https://fixture.test", RequestOptions{DisableChallengeReplay: true}); !errors.Is(err, ErrBrowserChallenge) || calls != 1 {
			t.Fatalf("security read replayed HTTP rejection: calls=%d err=%v", calls, err)
		}
	})
	t.Run("single attempt GET remains single", func(t *testing.T) {
		calls := 0
		client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
			calls++
			return nil, context.DeadlineExceeded
		}), nil, "")
		client.ConnectionRetries, client.RetryDelay = 3, 0
		if _, err := client.Request(t.Context(), http.MethodGet, "https://fixture.test", RequestOptions{SingleAttempt: true}); !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
			t.Fatalf("single attempt contract lost: calls=%d err=%v", calls, err)
		}
	})
}

func TestAuthorizationNavigationRetriesOnlyBeforeDispatch(t *testing.T) {
	unsent := unsentResetNetworkError(t)
	for _, rawURL := range []string{"https://fixture.test/api/auth/callback/openai", "https://fixture.test/callback?code=one-time", "https://fixture.test/signin?token=one-time"} {
		for _, scenario := range []string{"unknown", "unsent", "challenge"} {
			t.Run(rawURL+"/"+scenario, func(t *testing.T) {
				calls := 0
				client := NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
					calls++
					if !request.RetryBeforeSend || request.RetryReadOnly {
						t.Fatal("one-time navigation used read-only replay policy")
					}
					if scenario == "unsent" && calls < 3 {
						return nil, unsent
					}
					if scenario == "unknown" {
						return nil, io.EOF
					}
					if scenario == "challenge" {
						r := response(403, "Provider verification required")
						r.Header.Set("Cf-Mitigated", "challenge")
						return r, nil
					}
					return response(200, "{}"), nil
				}), nil, "")
				client.ConnectionRetries, client.RetryDelay = 3, 0
				client.Challenge = func(context.Context, string) error { t.Fatal("one-time navigation replayed a rejection"); return nil }
				_, err := client.Follow(t.Context(), rawURL, RequestOptions{RetryReadOnly: true})
				if scenario == "unsent" {
					if err != nil || calls != 3 {
						t.Fatal("unsent one-time navigation did not recover", calls, err)
					}
				} else if err == nil || calls != 1 {
					t.Fatal("one-time navigation was repeated after possible dispatch", calls, err)
				}
			})
		}
	}
}

func TestEachRedirectReadHasIndependentNetworkRetryBudget(t *testing.T) {
	counts := map[string]int{}
	client := NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
		counts[request.URL]++
		if counts[request.URL] < 3 {
			return nil, io.EOF
		}
		if request.URL == "https://fixture.test/start" {
			r := response(302, "")
			r.Header.Set("Location", "/next")
			return r, nil
		}
		return response(200, "{}"), nil
	}), nil, "")
	client.ConnectionRetries, client.RetryDelay = 3, 0
	_, err := client.Follow(t.Context(), "https://fixture.test/start", RequestOptions{})
	if err != nil || counts["https://fixture.test/start"] != 3 || counts["https://fixture.test/next"] != 3 {
		t.Fatal("only the first request received retries", counts, err)
	}
}
