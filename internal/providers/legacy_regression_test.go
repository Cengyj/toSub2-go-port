package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLegacySMSBowerOptionsAndLeaseLifecycle(t *testing.T) {
	checks := 0
	statuses := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("api_key") != "test-api-key" {
			t.Error("API key not sent")
		}
		switch q.Get("action") {
		case "getNumber":
			if q.Get("service") != "dr" || q.Get("country") != "1001" || q.Get("maxPrice") != "0.42" {
				t.Error("number options changed")
			}
			fmt.Fprint(w, "ACCESS_NUMBER:activation-1:60123456789")
		case "getStatus":
			checks++
			if q.Get("id") != "activation-1" {
				t.Error("wrong lease ID")
			}
			if checks == 1 {
				fmt.Fprint(w, "STATUS_WAIT_CODE")
			} else {
				fmt.Fprint(w, "STATUS_OK:OpenAI code 654321")
			}
		case "setStatus":
			statuses = append(statuses, q.Get("status"))
			fmt.Fprint(w, map[string]string{"1": "ACCESS_READY", "6": "ACCESS_ACTIVATION", "8": "ACCESS_CANCEL"}[q.Get("status")])
		case "getPrices":
			fmt.Fprint(w, `{"12":{"dr":{"cost":0.004,"count":366063}},"1001":{"dr":{"cost":0.42,"count":12}},"7":{"dr":{"cost":0.18,"count":8}},"0":{"dr":{"cost":0.08,"count":0}}}`)
		case "getCountries":
			fmt.Fprint(w, `{"data":[{"id":12,"eng":"United States (virtual)","chn":"美国（虚拟号码）"},{"id":1001,"eng":"Japan","chn":"日本"},{"id":7,"eng":"Malaysia","chn":"马来西亚"},{"id":0,"chn":"无库存"}]}`)
		default:
			t.Error("unexpected provider action")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	p := NewSMSBowerProvider("test-api-key", server.URL, "dr", "1001")
	p.MaxPrice = "0.42"
	ctx := context.Background()
	lease, err := p.GetNumber(ctx)
	if err != nil || lease.ID != "activation-1" || lease.Number != "+60123456789" {
		t.Fatal("lease normalization failed", err)
	}
	if _, err := p.GetCode(ctx, lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("waiting not preserved", err)
	}
	if code, err := p.GetCode(ctx, lease); err != nil || code != "654321" {
		t.Fatal("code not extracted", err)
	}
	for _, action := range []func(context.Context, NumberLease) error{p.MarkReady, p.Complete, p.Release} {
		if err := action(ctx, lease); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(statuses, []string{"1", "6", "8"}) {
		t.Fatal("wrong lease lifecycle")
	}
	options, err := p.ListNumberOptions(ctx, "dr")
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"country": "12", "title": "美国（虚拟号码）", "iso": "", "prefix": "", "price": 0.004, "count": 366063},
		{"country": "7", "title": "马来西亚", "iso": "", "prefix": "", "price": 0.18, "count": 8},
		{"country": "1001", "title": "日本", "iso": "", "prefix": "", "price": 0.42, "count": 12},
	}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("price ordering/stock/labels mismatch: %v", options)
	}
}

func TestLegacySMSBowerRejectsMissingNumberAndNonSixDigitCode(t *testing.T) {
	for _, body := range []string{"NO_NUMBERS", "STATUS_OK:order 123456789", "STATUS_OK:secret=test-api-key"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		p := NewSMSBowerProvider("test-api-key", server.URL, "dr", "1001")
		var err error
		if body == "NO_NUMBERS" {
			_, err = p.GetNumber(context.Background())
		} else {
			_, err = p.GetCode(context.Background(), NumberLease{ID: "fixture"})
		}
		server.Close()
		if err == nil || strings.Contains(err.Error(), "test-api-key") {
			t.Fatal("invalid provider response accepted or leaked")
		}
	}
}

func TestLegacyCustomPoolValidationAndTimestampedReplacement(t *testing.T) {
	for _, line := range []string{"8613711111111----https://sms.example/code", "+8613711111111----ftp://sms.example/code"} {
		if NewCustomPoolValue(line).Validate() == nil {
			t.Fatal("invalid phone or URL accepted")
		}
	}
	entries := make([]string, 501)
	for i := range entries {
		entries[i] = fmt.Sprintf("+1202555%04d----https://sms.example/code", i)
	}
	if NewCustomPool(entries[:500]).Validate() != nil || NewCustomPool(entries).Validate() == nil {
		t.Fatal("500-entry boundary incorrect")
	}
	checks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks++
		stamp := time.Now().Add(-time.Hour)
		if checks > 2 {
			stamp = time.Now().Add(time.Second)
		}
		fmt.Fprintf(w, `{"code":1,"msg":"ok","data":{"code":"您的验证代码是：766448","code_time":%q,"expired_date":"2099-08-25 00:00:00"}}`, stamp.UTC().Format(time.RFC3339Nano))
	}))
	defer server.Close()
	p := NewCustomPoolValue("+8613912345678----" + server.URL)
	lease, err := p.GetNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetCode(context.Background(), lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("stale timestamp accepted", err)
	}
	if code, err := p.GetCode(context.Background(), lease); err != nil || code != "766448" {
		t.Fatal("same code with new timestamp not recognized", err)
	}
	if err := p.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetCode(context.Background(), lease); err == nil {
		t.Fatal("released lease polled")
	}
}
