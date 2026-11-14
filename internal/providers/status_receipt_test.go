package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSMSBowerReceiptMustMatchRequestedAction(t *testing.T) {
	for _, action := range []struct {
		name, status, receipt string
		call                  func(*SMSBowerProvider, context.Context, NumberLease) error
	}{
		{"ready", "1", "ACCESS_READY", (*SMSBowerProvider).MarkReady},
		{"complete", "6", "ACCESS_ACTIVATION", (*SMSBowerProvider).Complete},
		{"release", "8", "ACCESS_CANCEL", (*SMSBowerProvider).Release},
	} {
		for _, receipt := range []string{"ACCESS_READY", "ACCESS_ACTIVATION", "ACCESS_CANCEL", "ACCESS_RETRY_GET"} {
			t.Run(action.name+"/"+receipt, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Query().Get("status") != action.status || r.URL.Query().Get("id") != "fixture-order" {
						t.Error("provider mutation lost action or order binding")
					}
					fmt.Fprint(w, receipt)
				}))
				defer server.Close()
				err := action.call(NewSMSBowerProvider("fixture-key", server.URL, "dr", "1"), t.Context(), NumberLease{ID: "fixture-order"})
				if (err == nil) != (receipt == action.receipt) || calls.Load() != 1 {
					t.Fatalf("receipt result err=%v calls=%d", err, calls.Load())
				}
			})
		}
	}
}
