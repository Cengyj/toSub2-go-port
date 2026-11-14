package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestFailureDiagnosticKeepsOnlyKnownCodesAndObservedHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{"permission", fmt.Errorf("synthetic-private upstream body: %w", &Error{Code: "forbidden", HTTPStatus: 403}), "forbidden", 403},
		{"stream", &Error{Code: "incomplete", HTTPStatus: 200}, "incomplete", 200},
		{"network", &Error{Code: "network_error"}, "network_error", 0},
		{"unknown-code", &Error{Code: "synthetic-private https://private.example.test", HTTPStatus: 403}, "probe_failed", 0},
		{"unknown-error", errors.New("synthetic-private Authorization: Bearer secret"), "probe_failed", 0},
		{"invalid-status", &Error{Code: "http_error", HTTPStatus: 999}, "http_error", 0},
		{"canceled", fmt.Errorf("synthetic-private: %w", context.Canceled), "canceled", 0},
		{"timeout", context.DeadlineExceeded, "timeout", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DiagnoseFailure(tc.err)
			if d.Code != tc.code || d.HTTPStatus != tc.status || d.Message == "" || strings.Contains(d.Message, "model probe") {
				t.Fatalf("incomplete public diagnostic: %+v", d)
			}
			encoded, _ := json.Marshal(d)
			if strings.Contains(string(encoded), "synthetic-private") || strings.Contains(string(encoded), "Authorization") || strings.Contains(string(encoded), "https://") {
				t.Fatal("diagnostic exposed untrusted text")
			}
		})
	}
	for _, code := range []string{"unauthorized", "forbidden", "rate_limited", "invalid_response", "network_error", "timeout", "incomplete"} {
		failure := &Error{Code: code}
		if failure.Error() != DiagnoseFailure(failure).Message {
			t.Fatal("synchronous and recorded failure messages differ")
		}
	}
}

func TestProbeValidationFailuresRetainHTTP200WithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		response   func() *transport.Response
		manual     bool
	}{
		{"stream-incomplete", "incomplete", func() *transport.Response {
			return &transport.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte("data: [DONE]\n\n")}
		}, false},
		{"wrong-answer-format", "invalid_answer_format", func() *transport.Response { return successfulResponse("synthetic-private explanation") }, false},
		{"incomplete-animation", "invalid_response", func() *transport.Response {
			return successfulResponse(`<html><body><script>synthetic-private`)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return tc.response(), nil
			})}
			var err error
			if tc.manual {
				_, err = client.Pelican(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
			} else {
				_, err = client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
			}
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.HTTPStatus != 200 || calls != 1 {
				t.Fatalf("response status lost or request retried: %#v calls=%d", failure, calls)
			}
			if strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("invalid model text escaped")
			}
		})
	}
}

func TestProbeTransportFailureRetainsReceivedStatusAndClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{errors.New("synthetic-private body read failure"), "network_error"},
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "canceled"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			calls := 0
			client := &Client{Transport: fixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return &transport.Response{StatusCode: 503}, tc.err
			})}
			result, err := client.Probe(t.Context(), testCredentials(), Options{Model: "synthetic-model"})
			failure := requireProbeError(t, result, err, tc.code)
			if calls != 1 || failure.HTTPStatus != 503 {
				t.Fatal("observed HTTP status lost or request retried")
			}
		})
	}
}
