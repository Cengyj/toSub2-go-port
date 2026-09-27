package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderAPIKeyValidationPreventsNetworkRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("invalid credential reached provider")
	}))
	defer server.Close()
	for _, key := range []string{"", "short", "secret?value", "secret\nvalue", strings.Repeat("k", 257)} {
		for _, p := range []SmsProvider{
			&HTTPProvider{APIKey: key, Base: server.URL, Service: "121949"},
			NewSMSBowerProvider(key, server.URL, "dr", "1"),
		} {
			_, err := p.GetNumber(context.Background())
			if !IsTerminal(err) || (key != "" && strings.Contains(err.Error(), key)) {
				t.Fatalf("invalid key rejection was transient or exposed credential: %T %v", p, err)
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid API key was sent over network")
	}
}

func TestLubanLegacyCodeCandidatePriorityAndTimestamps(t *testing.T) {
	cutoff := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC).UnixMilli()
	for _, tc := range []struct {
		name, response, code string
		stale                bool
	}{
		{"sms-message-without-keywords", `{"code":0,"sms_msg":"Use 123456 to continue"}`, "123456", false},
		{"status-message-without-keywords", `{"code":0,"msg":"Use 000123 to continue"}`, "000123", false},
		{"nested-number-without-keywords", `{"code":0,"data":{"value":123456}}`, "123456", false},
		{"exact-code-priority", `{"code":0,"sms_msg":"Use 123456 to continue","message":"654321"}`, "654321", false},
		{"code-field-priority", `{"code":0,"sms_msg":"Use 123456 to continue","data":{"otp":"654321"}}`, "654321", false},
		{"explicit-sms-code-priority", `{"code":0,"sms_code":"000123","sms_msg":"654321","data":{"otp":"123456"}}`, "000123", false},
		{"stale-generic-message", `{"code":0,"msg":"Use 123456 to continue","received_at":"2026-09-25T23:59:00Z"}`, "", true},
		{"new-child-overrides-old-envelope", `{"code":0,"received_at":"2026-09-25T23:59:00Z","sms_code":"123456","messages":[{"value":"Use 654321 to continue","created_at":"2026-09-26T00:01:00Z"}]}`, "654321", false},
		{"metadata-is-not-code", `{"code":0,"request_id":"123456","number":"654321","api_key":"234567","received_at":"2026-09-26T00:00:00.654321Z"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal([]byte(tc.response), &data); err != nil {
				t.Fatal(err)
			}
			code, stale := freshLubanCode(data, cutoff)
			if code != tc.code || stale != tc.stale {
				t.Fatalf("code = %q, stale = %v; want %q, %v", code, stale, tc.code, tc.stale)
			}
		})
	}
}

func TestProviderAPIKeyNormalizationAndBaseURLResolution(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v2/api/getNumber":
			if r.URL.Query().Get("apikey") != "fixture-key" || r.URL.Query().Get("stale") != "" {
				t.Error("Luban key normalization or relative URL resolution changed")
			}
			fmt.Fprint(w, `{"code":0,"request_id":"lease","number":"12025550123"}`)
		case "/handler_api.php":
			q := r.URL.Query()
			if q.Get("api_key") != "fixture-key" || q.Get("route") != "retained" || q.Get("action") != "getNumber" || q.Get("service") != "dr" || len(q["api_key"]) != 1 || len(q["action"]) != 1 {
				t.Error("SMSBower did not retain base URL options or replace reserved values")
			}
			fmt.Fprint(w, "ACCESS_NUMBER:lease:12025550123")
		default:
			t.Errorf("wrong provider path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	for _, p := range []SmsProvider{
		&HTTPProvider{APIKey: "  fixture-key ", Base: server.URL + "/v2/api/?stale=value", Service: "121949"},
		&HTTPProvider{APIKey: "fixture-key", Base: server.URL + "/v2/api/previous", Service: "121949"},
		NewSMSBowerProvider("  fixture-key  ", server.URL+"/handler_api.php?route=retained&api_key=stale&api_key=also-stale&action=stale&service=old", "dr", "1"),
	} {
		if _, err := p.GetNumber(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 3 {
		t.Fatalf("provider requests = %d, want 3", requests.Load())
	}
}

func TestLubanUnclassifiedResponseRemainsRetryable(t *testing.T) {
	for _, tc := range []struct {
		body     string
		terminal bool
	}{
		{`{"code":0,"msg":"pending"}`, false},
		{`{"code":0,"msg":"success"}`, true},
		{`{"code":0,"sms_code":"invalid"}`, true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, tc.body)
		}))
		p := &HTTPProvider{APIKey: "fixture-key", Base: server.URL}
		_, err := p.GetCode(context.Background(), NumberLease{ID: "lease"})
		server.Close()
		if err == nil || IsTerminal(err) != tc.terminal {
			t.Fatalf("Luban response retry classification changed: %v", err)
		}
	}
}

func TestSMSBowerOptionsPreserveProviderErrorClassification(t *testing.T) {
	for _, endpoint := range []string{"getPrices", "getCountries"} {
		for _, response := range []string{"BAD_KEY", "BANNED:private-key-value", "malformed private-key-value"} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("action") == endpoint {
					fmt.Fprint(w, response)
				} else if r.URL.Query().Get("action") == "getPrices" {
					fmt.Fprint(w, `{"1":{"dr":{"cost":0.1,"count":2}}}`)
				} else {
					fmt.Fprint(w, `{}`)
				}
			}))
			_, err := NewSMSBowerProvider("fixture-key", server.URL, "dr", "1").ListNumberOptions(context.Background(), "dr")
			server.Close()
			if err == nil || IsTerminal(err) != !strings.HasPrefix(response, "malformed") || strings.Contains(err.Error(), "private-key-value") {
				t.Fatalf("%s error lost classification or exposed response: %v", endpoint, err)
			}
		}
	}
	for _, response := range []string{"[]", "null", "true"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) }))
		_, err := NewSMSBowerProvider("fixture-key", server.URL, "dr", "1").ListNumberOptions(context.Background(), "dr")
		server.Close()
		if !IsTerminal(err) {
			t.Fatal("invalid price envelope must be terminal", err)
		}
	}
}
