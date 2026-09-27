package mail

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type failureTransport struct{ err error }

func (f failureTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }
func TestMailNetworkErrorHidesCredentialURLAndRetainsCause(t *testing.T) {
	cause := errors.New("fixture dial failed")
	client := &http.Client{Transport: failureTransport{cause}}
	_, err := FetchCandidates(context.Background(), "https://mail.example/private-path-secret?token=private-query-secret", RequestConfig{}, client)
	if err == nil || !errors.Is(err, cause) {
		t.Fatal("network failure cause missing")
	}
	if strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "mail.example") {
		t.Fatal("mail URL credential leaked through error")
	}
}
