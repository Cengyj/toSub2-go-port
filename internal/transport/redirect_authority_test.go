package transport

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Client.Do promises one observable HTTP response. A caller-supplied native
// client must not hide an intermediate response or replay its request body.
func TestNativeHTTPKeepsRedirectsObservableWithInjectedClient(t *testing.T) {
	for _, singleAttempt := range []bool{false, true} {
		t.Run(fmt.Sprint(singleAttempt), func(t *testing.T) {
			var redirected, policyCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/start" {
					w.Header().Set("Location", "/destination")
					w.Header().Set("Set-Cookie", "receipt=accepted; Path=/")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				redirected.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			caller := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
				policyCalls.Add(1)
				return nil
			}}
			client := NewClient(nil)
			defer client.Close()
			client.HTTP = caller
			response, err := client.Do(t.Context(), Request{Method: http.MethodPost, URL: server.URL + "/start", Body: []byte("synthetic-operation"), SingleAttempt: singleAttempt})
			if err != nil || response == nil || response.StatusCode != http.StatusTemporaryRedirect || redirected.Load() != 0 || policyCalls.Load() != 0 {
				t.Fatalf("native client hid or followed a redirect: status=%d redirected=%d policyCalls=%d err=%v", response.Status(), redirected.Load(), policyCalls.Load(), err)
			}
			if client.Jar.Value("receipt", server.URL) != "accepted" {
				t.Fatal("intermediate response cookie was not retained")
			}
			if caller.CheckRedirect == nil || caller.CheckRedirect(nil, nil) != nil || policyCalls.Load() != 1 {
				t.Fatal("adapter changed the caller-owned redirect policy")
			}
		})
	}
}
