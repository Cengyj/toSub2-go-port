package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestSMSRetryUsesActionSemantics(t *testing.T) {
	for _, action := range []string{"getStatus", "getNumber", "setStatus"} {
		t.Run(action, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("action") != action {
					t.Error("unexpected action")
				}
				if calls.Add(1) <= 2 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				_, _ = fmt.Fprint(w, "STATUS_OK:123456")
			}))
			defer server.Close()
			p := NewSMSBowerProvider("fixture-key", server.URL, "dr", "1")
			_, err := p.call(t.Context(), action, nil)
			if action == "getStatus" {
				if err != nil || calls.Load() != 3 {
					t.Fatal("safe SMS query did not recover", err)
				}
			} else if err == nil || calls.Load() != 1 {
				t.Fatal("SMS purchase/status GET was replayed after reaching server", err)
			}
		})
	}
}

func TestSMSHTTPRejectionStopsPolling(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, err := PollCode(context.Background(), NewSMSBowerProvider("fixture-key", server.URL, "dr", "1"), NumberLease{ID: "fixture"}, time.Millisecond, time.Second)
	if err == nil || !IsTerminal(err) || calls.Load() != 1 {
		t.Fatal("HTTP rejection was treated as transient network failure", err)
	}
}

func TestLubanBusinessFailuresStopPollingButPendingDoesNot(t *testing.T) {
	for _, firstResponse := range []string{`malformed fixture-private-key`, `{"code":503,"msg":"fixture-private-key"}`, `{"code":"402"}`, `{"code":0,"msg":"pending"}`} {
		t.Run(firstResponse, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					_, _ = fmt.Fprint(w, firstResponse)
					return
				}
				_, _ = fmt.Fprint(w, `{"code":0,"sms_code":"123456"}`)
			}))
			defer server.Close()
			provider := &HTTPProvider{APIKey: "fixture-key", Base: server.URL, OnRetry: func(transport.ReadRetryEvent) {
				t.Error("business response triggered a network retry")
			}}
			code, err := PollCode(t.Context(), provider, NumberLease{ID: "fixture"}, time.Millisecond, time.Second)
			if firstResponse == `{"code":0,"msg":"pending"}` {
				if err != nil || code != "123456" || calls.Load() != 2 {
					t.Fatal("normal pending response stopped polling", err)
				}
			} else if err == nil || !IsTerminal(err) || calls.Load() != 1 {
				t.Fatal("invalid JSON or numeric business failure was polled again", err)
			}
		})
	}
}
