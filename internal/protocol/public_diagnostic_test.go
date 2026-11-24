package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestUnexpectedLoginPageDoesNotExposeRemotePath(t *testing.T) {
	h := newRecoveryHarness(t)
	const privatePath = "/opaque-remote-login-material"
	requests := 0
	h.intercept = func(_ context.Context, request transport.Request) (*transport.Response, error, bool) {
		requests++
		switch pathOf(request.URL) {
		case "/authorize":
			return fixtureRedirect("https://auth.example" + privatePath + "?session=synthetic"), nil, true
		case privatePath:
			return response(200, "unrecognized login page"), nil, true
		}
		return nil, nil, false
	}
	_, err := h.flow.Run(t.Context())
	if err == nil || !strings.HasPrefix(err.Error(), "UNEXPECTED_LOGIN_PAGE:") {
		t.Fatal("unexpected login page lost its diagnosis", err)
	}
	if strings.Contains(err.Error(), privatePath) || strings.Contains(err.Error(), "auth.example") || strings.Contains(err.Error(), "synthetic") {
		t.Fatal("unrecognized upstream path entered a public error")
	}
	if requests != 6 || h.loginStarts != 1 || h.resets != 0 || h.oauthRequests != 0 || h.tokenRequests != 0 {
		t.Fatal("diagnostic handling changed login requests or allowed later authorization", requests, h.loginStarts, h.resets, h.oauthRequests, h.tokenRequests)
	}
}

func TestJSONResponseTypeFailureKeepsClassificationWithoutRemoteValue(t *testing.T) {
	const privateNumber = "9876543210987654321098765432109876543210e999"
	calls := 0
	c := NewClient(doerFunc(func(_ context.Context, _ transport.Request) (*transport.Response, error) {
		calls++
		return response(200, `{"metadata":`+privateNumber+`}`), nil
	}), nil, "")
	var observed error
	c.OnRequestResult = func(_ string, status int, err error) {
		if err != nil {
			if status != 200 {
				t.Fatal("parse failure lost its actual HTTP response")
			}
			observed = err
		}
	}
	for _, ignored := range []bool{false, true} {
		var decoded map[string]any
		var out any = &decoded
		if ignored {
			out = nil
		}
		r, err := c.JSON(t.Context(), "POST", "https://auth.example/fixture", map[string]any{"intent": true}, RequestOptions{}, out)
		var typed *json.UnmarshalTypeError
		if err == nil || r == nil || r.StatusCode != 200 || !errors.As(err, &typed) || observed != err {
			t.Fatal("parse failure lost response or typed classification", err)
		}
		if !strings.Contains(err.Error(), "HTTP 200") || strings.Contains(err.Error(), privateNumber) {
			t.Fatal("remote JSON value entered the public parse diagnostic")
		}
	}
	if calls != 2 {
		t.Fatal("JSON parse failure repeated the request", calls)
	}
}
