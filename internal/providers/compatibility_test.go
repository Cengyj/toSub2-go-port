package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSMSBowerNumberOptionsValidateBeforePurchase(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "ACCESS_NUMBER:fixture:12025550123")
	}))
	defer server.Close()
	for _, tc := range []struct{ service, country, price string }{
		{"dr/other", "1", ""}, {"dr", "-1", ""}, {"dr", "1.2", ""}, {"dr", "123456", ""}, {"dr", "1", "NaN"}, {"dr", "1", "Inf"}, {"dr", "1", "0"}, {"dr", "1", "1e3"}, {"dr", "1", "0.1234567"},
	} {
		p := NewSMSBowerProvider("fixture-key", server.URL, tc.service, tc.country)
		p.MaxPrice = tc.price
		if _, err := p.GetNumber(context.Background()); !IsTerminal(err) {
			t.Errorf("invalid options did not fail terminally: %+v %v", tc, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid configuration reached the purchase endpoint")
	}
	p := NewSMSBowerProvider("fixture-key", server.URL, " DR ", " 1 ")
	p.MaxPrice = " 0.123456 "
	if _, err := p.GetNumber(context.Background()); err != nil || requests.Load() != 1 {
		t.Fatalf("normalized valid options rejected: %v", err)
	}
}

func TestLubanNumberOptionsValidateBeforePurchase(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"code":0,"request_id":"fixture","number":"12025550123"}`)
	}))
	defer server.Close()
	for _, service := range []string{"", "service&other=1", strings.Repeat("a", 81)} {
		p := &HTTPProvider{Base: server.URL, Service: service}
		if _, err := p.GetNumber(context.Background()); !IsTerminal(err) {
			t.Fatal("invalid service did not fail terminally", err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid configuration reached the purchase endpoint")
	}
}

func TestSMSBowerPriceAliasesFallbacksAndNonFiniteNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "getCountries" {
			fmt.Fprint(w, `[{"id":1007,"activate_org_code":7,"eng":"Malaysia","chn":"马来西亚","iso":"my"}]`)
			return
		}
		fmt.Fprint(w, `{"Malaysia":{"dr":{"cost":"0.1","count":"25"}},"12":{"dr":{"cost":0.2,"count":12}},"18":{"dr":{"cost":"NaN","count":1}},"19":{"dr":{"cost":1,"count":"Inf"}},"20":{"dr":{"cost":"Inf","count":1}},"21":{"dr":{"cost":1,"count":"1e30"}}}`)
	}))
	defer server.Close()
	options, err := NewSMSBowerProvider("fixture-key", server.URL, "dr", "1").ListNumberOptions(context.Background(), " DR ")
	if err != nil || len(options) != 2 || options[0]["country"] != "7" || options[1]["title"] != "美国（虚拟号码）" {
		t.Fatalf("invalid options: %v %v", options, err)
	}
	if _, err := json.Marshal(options); err != nil {
		t.Fatal("options contain non-JSON numeric data", err)
	}
}

func TestSMSProviderTerminalClassificationNeverLeaksBody(t *testing.T) {
	for _, tc := range []struct {
		body     string
		terminal bool
	}{
		{"BAD_KEY", true}, {"NO_BALANCE", true}, {"NO_ACTIVATION", true}, {"STATUS_CANCEL", true}, {"BANNED:fixture-private-key", true}, {"STATUS_OK:not-a-code fixture-private-key", true},
		{"NO_NUMBERS", false}, {"EARLY_CANCEL_DENIED", false}, {"unexpected fixture-private-key", false},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
		_, err := NewSMSBowerProvider("fixture-key", server.URL, "dr", "1").GetCode(context.Background(), NumberLease{ID: "fixture"})
		server.Close()
		if err == nil || IsTerminal(err) != tc.terminal || strings.Contains(err.Error(), "private") {
			t.Fatalf("classification or redaction mismatch: %v %v", tc, err)
		}
	}
}

func TestPollCodeRetriesTemporaryFailuresAndStopsTerminal(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				if terminal {
					fmt.Fprint(w, "NO_ACTIVATION")
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				return
			}
			fmt.Fprint(w, "STATUS_OK:123456")
		}))
		code, err := PollCode(context.Background(), NewSMSBowerProvider("fixture-key", server.URL, "dr", "1"), NumberLease{ID: "fixture"}, time.Millisecond, time.Second)
		server.Close()
		if terminal {
			if !IsTerminal(err) || calls.Load() != 1 || code != "" {
				t.Fatal("terminal failure retried", err)
			}
		} else if err != nil || calls.Load() != 2 || code != "123456" {
			t.Fatal("temporary failure did not retry", err)
		}
	}
}

func TestPollCodeCanceledBeforeRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A nil provider makes any accidental attempt to perform a request fail.
	if _, err := PollCode(ctx, nil, NumberLease{}, time.Millisecond, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCustomPoolRestoreRejectsUnrelatedOrUnallocatedLease(t *testing.T) {
	entries := []string{"+12025550123----https://sms.example/first", "+12025550124----https://sms.example/second"}
	for _, tc := range []struct {
		name   string
		mutate func(*CustomPoolState)
	}{
		{"other-pool", func(s *CustomPoolState) { s.Entries[0] = "+12025550123----https://other.example/first" }},
		{"unallocated-url", func(s *CustomPoolState) {
			v := s.Leases["custom-fixture"]
			v.URL = "https://sms.example/second"
			s.Leases["custom-fixture"] = v
		}},
		{"invalid-time", func(s *CustomPoolState) {
			v := s.Leases["custom-fixture"]
			v.Requested = -1
			s.Leases["custom-fixture"] = v
		}},
		{"unallocated-lease", func(s *CustomPoolState) { s.Next = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewCustomPool(entries)
			s := CustomPoolState{Entries: append([]string(nil), entries...), Next: 1, Leases: map[string]CustomLeaseState{"custom-fixture": {URL: "https://sms.example/first", Seen: map[string]bool{}, Requested: time.Now().UnixMilli()}}}
			tc.mutate(&s)
			if err := p.RestorePrivateState(s); err == nil || p.PrivateState().Next != 0 {
				t.Fatal("invalid restore was accepted or partially applied", err)
			}
		})
	}
}
