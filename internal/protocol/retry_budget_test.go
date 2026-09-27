package protocol

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestConfiguredReadRetryBudgetAndZeroDelay(t *testing.T) {
	for _, retries := range []int{0, 4, 20} {
		calls := 0
		failure := errors.New("synthetic connection closed")
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
		failure := errors.New("connection closed after sending")
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
