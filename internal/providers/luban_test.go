package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLubanHTTPLeasePollingReleaseAndComplete(t *testing.T) {
	const key = "synthetic-luban-api-key"
	var smsCalls, releaseCalls, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.Query().Get("apikey") != key {
			t.Error("Luban request authentication or method incorrect")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/api/getNumber":
			if r.URL.Query().Get("service_id") != "121949" {
				t.Error("Luban service_id missing")
			}
			fmt.Fprint(w, `{"code":"0","msg":"","number":"00 60 (123) 456-789","request_id":"lease-fixture"}`)
		case "/v2/api/getSms":
			if r.URL.Query().Get("request_id") != "lease-fixture" {
				t.Error("Luban lease ID missing")
			}
			if smsCalls.Add(1) == 1 {
				fmt.Fprint(w, `{"code":0,"msg":"wait","sms_msg":{"request_id":"123456"}}`)
			} else {
				fmt.Fprint(w, `{"code":0,"msg":"success","sms_code":{"code":"654321"},"sms_msg":{"request_id":"987654","text":"Alternate old code 111111"}}`)
			}
		case "/v2/api/setStatus":
			releaseCalls.Add(1)
			if r.URL.Query().Get("status") != "reject" || r.URL.Query().Get("request_id") != "lease-fixture" {
				t.Error("Luban release parameters incorrect")
			}
			fmt.Fprint(w, `{"code":0,"msg":"success"}`)
		default:
			t.Errorf("unexpected Luban endpoint: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p := &HTTPProvider{APIKey: key, Base: server.URL + "/v2/api/", Service: "121949"}
	lease, e := p.GetNumber(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if lease.Number != "+60123456789" || lease.Provider != "luban" || lease.ID != "lease-fixture" {
		t.Fatalf("lease normalization incorrect: %+v", lease)
	}
	if lease.Metadata["requested_at"] == "" {
		t.Fatal("lease timestamp not recorded")
	}
	if _, e = p.GetCode(context.Background(), lease); !errors.Is(e, ErrWaiting) {
		t.Fatalf("waiting response: %v", e)
	}
	code, e := p.GetCode(context.Background(), lease)
	if e != nil || code != "654321" {
		t.Fatalf("nested explicit sms_code priority: %q %v", code, e)
	}
	before := requests.Load()
	if e = p.Complete(context.Background(), lease); e != nil {
		t.Fatal(e)
	}
	if requests.Load() != before {
		t.Fatal("Luban completion invented unsupported network request")
	}
	if e = p.Release(context.Background(), lease); e != nil {
		t.Fatal(e)
	}
	if releaseCalls.Load() != 1 {
		t.Fatal("Luban lease not released")
	}
	encoded, _ := json.Marshal(lease)
	if strings.Contains(string(encoded), key) {
		t.Fatal("API key leaked into lease metadata")
	}
}

func TestLubanRejectsOldSMSAndIdentifierDigits(t *testing.T) {
	cutoff := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	var response atomic.Value
	response.Store(`{"code":0,"msg":"success","sms_code":"123456","received_at":"2026-09-25T11:59:00Z"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response.Load().(string)) }))
	defer server.Close()
	p := &HTTPProvider{Base: server.URL, APIKey: "synthetic-only"}
	lease := NumberLease{ID: "fixture", Metadata: map[string]string{"requested_at": strconv.FormatInt(cutoff.UnixMilli(), 10)}}
	if _, e := p.GetCode(context.Background(), lease); !errors.Is(e, ErrWaiting) {
		t.Fatalf("old SMS accepted: %v", e)
	}
	response.Store(`{"code":0,"msg":"success","messages":[{"text":"OpenAI code: 111111","received_at":"2026-09-25T11:59:00Z"},{"text":"OpenAI code: 222222","received_at":"2026-09-25T12:00:01Z"}]}`)
	if code, e := p.GetCode(context.Background(), lease); e != nil || code != "222222" {
		t.Fatalf("fresh message not selected: %q %v", code, e)
	}
	for _, body := range []string{`{"code":0,"msg":"success","request_id":"123456","number":"654321","api_key":"234567"}`, `{"code":0,"msg":"success","sms_code":"123456789"}`} {
		response.Store(body)
		if code, e := p.GetCode(context.Background(), lease); e == nil || code != "" {
			t.Fatal("identifier or non-six-digit value accepted as OTP")
		}
	}
	response.Store(`{"code":0,"msg":"success","sms_code":"000123"}`)
	if code, e := p.GetCode(context.Background(), lease); e != nil || code != "000123" {
		t.Fatalf("leading-zero six-digit code rejected: %q %v", code, e)
	}
}

func TestLubanProviderErrorsNeverEchoAPIKeyOrOTP(t *testing.T) {
	const key = "SYNTHETICAPIKEYSECRET"
	for _, tc := range []struct {
		name, body string
		status     int
	}{{"status", `{"code":400,"msg":"` + key + ` otp 123456"}`, 200}, {"echoed-code", `{"code":"` + key + `","msg":"rejected"}`, 200}, {"malformed", `not-json ` + key, 200}, {"http", key, 503}, {"invalid-number", `{"code":0,"request_id":"fixture","number":"bad"}`, 200}, {"redirect", key, 302}} {
		t.Run(tc.name, func(t *testing.T) {
			var followed atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/leak" {
					followed.Add(1)
				}
				w.Header().Set("Location", "/leak")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			p := &HTTPProvider{APIKey: key, Base: server.URL, Service: "121949"}
			_, e := p.GetNumber(context.Background())
			if e == nil {
				t.Fatal("provider error accepted")
			}
			if strings.Contains(e.Error(), key) || strings.Contains(e.Error(), "123456") {
				t.Fatal("credential or OTP leaked into error")
			}
			if followed.Load() != 0 {
				t.Fatal("provider redirect followed")
			}
		})
	}
}

func TestLubanCancellationStopsRequest(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(finished) }))
	defer server.Close()
	p := &HTTPProvider{APIKey: "synthetic", Base: server.URL, Service: "121949"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, e := p.GetNumber(ctx); result <- e }()
	<-entered
	cancel()
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("cancellation lost: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not stop")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("provider connection not canceled")
	}
}
