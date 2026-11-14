package protocol

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestClientObservationKeepsStatusWhenResponseBodyFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "short")
	}))
	defer server.Close()
	client := NewClient(nil, nil, "auto")
	var statuses []int
	client.OnRequestResult = func(raw string, status int, _ error) {
		if raw != server.URL {
			t.Error("observer changed request identity")
		}
		statuses = append(statuses, status)
	}
	headerCalls := 0
	_, err := client.Request(t.Context(), http.MethodPost, server.URL, RequestOptions{SingleAttempt: true, OnResponseHeaders: func(r *transport.Response) error {
		headerCalls++
		if r.StatusCode != http.StatusForbidden {
			t.Error("original response callback changed")
		}
		return nil
	}})
	if err == nil || !reflect.DeepEqual(statuses, []int{403}) || headerCalls != 1 {
		t.Fatal("partial response status lost", statuses, headerCalls, err)
	}
}

func TestClientObservationRetainsPersistenceBarrierAndDoesNotReplay(t *testing.T) {
	cause := errors.New("fixture response receipt could not persist")
	for _, single := range []bool{false, true} {
		calls, headers := 0, 0
		client := NewClient(doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
			calls++
			err := request.OnResponseHeaders(&transport.Response{StatusCode: 503})
			return nil, err
		}), nil, "auto")
		var statuses []int
		client.OnRequestResult = func(_ string, status int, _ error) { statuses = append(statuses, status) }
		client.ConnectionRetries = 3
		_, err := client.Request(t.Context(), http.MethodPost, "https://fixture.example/change", RequestOptions{SingleAttempt: single, OnResponseHeaders: func(*transport.Response) error { headers++; return cause }})
		if err != cause || calls != 1 || headers != 1 || !reflect.DeepEqual(statuses, []int{503}) {
			t.Fatal("observation changed response barrier or replay policy", calls, headers, statuses, err)
		}
	}
}

func TestClientObservationDoesNotTreatHTTPRejectionAsTransportRetry(t *testing.T) {
	calls := 0
	client := NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		return response(429, `{"error":"rate_limited"}`), nil
	}), nil, "auto")
	client.ConnectionRetries = 3
	var statuses []int
	client.OnRequestResult = func(_ string, status int, _ error) { statuses = append(statuses, status) }
	_, err := client.JSON(t.Context(), http.MethodGet, "https://fixture.example/info", nil, RequestOptions{}, nil)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 429 || calls != 1 || !reflect.DeepEqual(statuses, []int{429, 429}) {
		t.Fatal("observation changed HTTP failure", calls, statuses, err)
	}
}
