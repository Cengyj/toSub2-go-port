package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLegacyLubanNumericPhoneAndOrderIDs(t *testing.T) {
	for _, tc := range []struct {
		name, response, id string
	}{
		{"numeric-phone", `{"code":0,"number":12025550123,"request_id":"synthetic-lease"}`, "synthetic-lease"},
		{"numeric-id", `{"code":0,"number":"12025550123","request_id":987654321}`, "987654321"},
		{"large-exact-numeric-id", `{"code":0,"number":12025550123,"request_id":9007199254740991}`, "9007199254740991"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/getNumber":
					fmt.Fprint(w, tc.response)
				case "/getSms", "/setStatus":
					if id := r.URL.Query().Get("request_id"); id != tc.id {
						t.Errorf("order ID changed on provider request: got %q, want %q", id, tc.id)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if r.URL.Path == "/getSms" {
						fmt.Fprint(w, `{"code":0,"sms_code":"654321"}`)
					} else {
						if r.URL.Query().Get("status") != "reject" {
							t.Error("legacy release status changed")
						}
						fmt.Fprint(w, `{"code":0}`)
					}
				default:
					t.Errorf("unexpected provider endpoint %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			provider := &HTTPProvider{APIKey: "synthetic-key", Base: server.URL, Service: "121949"}
			lease, err := provider.GetNumber(context.Background())
			if err != nil || lease.ID != tc.id || lease.Number != "+12025550123" {
				t.Fatalf("numeric provider response was not preserved: lease=%#v, err=%v", lease, err)
			}
			if code, err := provider.GetCode(context.Background(), lease); err != nil || code != "654321" {
				t.Fatalf("numeric lease could not retrieve SMS: code=%q, error=%v", code, err)
			}
			if err := provider.Release(context.Background(), lease); err != nil {
				t.Fatal(err)
			}
		})
	}
}
