package mail

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestMailboxQueriesRetryConnectionAndTruncatedBody(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != method || method == http.MethodPost && string(body) != `{"query":"fixture"}` {
					t.Error("query method or body changed during retry")
				}
				attempt := calls.Add(1)
				if attempt <= 2 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if attempt == 2 {
						_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\ncut")
					}
					_ = conn.Close()
					return
				}
				_, _ = fmt.Fprint(w, `{"body":"OpenAI code 123456"}`)
			}))
			defer server.Close()
			events := []transport.ReadRetryEvent{}
			got, err := FetchCandidates(t.Context(), server.URL+"/?token=private-fixture", RequestConfig{Method: method, Body: `{"query":"fixture"}`, OnRetry: func(event transport.ReadRetryEvent) { events = append(events, event) }}, server.Client())
			if err != nil || len(got) != 1 || got[0].Code != "123456" || calls.Load() != 3 || len(events) != 2 {
				t.Fatalf("mail retry did not recover: candidates=%d requests=%d retries=%d error=%v", len(got), calls.Load(), len(events), err)
			}
		})
	}
}

func TestMailboxPermanentFailuresDoNotRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	_, err := FetchCandidates(t.Context(), server.URL, RequestConfig{}, server.Client())
	if err == nil || !IsTerminal(err) || calls.Load() != 1 {
		t.Fatal("HTTP rejection was retried or did not stop polling", err)
	}
	client := &http.Client{Transport: failureTransport{x509.UnknownAuthorityError{}}}
	_, err = FetchCandidates(t.Context(), "https://fixture.invalid/?token=private-fixture", RequestConfig{}, client)
	if err == nil || !IsTerminal(err) || strings.Contains(err.Error(), "private-fixture") {
		t.Fatal("certificate failure was not classified safely", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = FetchCandidates(ctx, server.URL, RequestConfig{}, server.Client())
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal("canceled mailbox query reached the server", err)
	}
}

func TestMailboxRedirectKeepsQueryAndDropsCrossOriginAuthentication(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("redirect retained POST or forwarded private mailbox authentication")
		}
		_, _ = fmt.Fprint(w, `{"body":"OpenAI code 123456"}`)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-private" {
			t.Error("original query lost its authentication")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	got, err := FetchCandidates(t.Context(), origin.URL, RequestConfig{Method: http.MethodPost, Body: `{"query":"fixture"}`, Headers: map[string]string{"Authorization": "Bearer fixture-private", "X-Api-Key": "fixture-private"}}, origin.Client())
	if err != nil || len(got) != 1 || targetCalls.Load() != 1 {
		t.Fatal("mail query redirect failed", err)
	}
}
