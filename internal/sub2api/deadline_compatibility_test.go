package sub2api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type deadlineProbeTransport struct {
	t    *testing.T
	want time.Duration
}

func (p deadlineProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining > p.want || remaining < p.want-time.Second {
		p.t.Errorf("Sub2API request budget = %s, want %s", remaining, p.want)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
}

func TestLegacySub2APIRequestDeadlineAndCallerCancellation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		configuredLimit time.Duration
		parentLimit     time.Duration
		want            time.Duration
	}{
		{"legacy-default", 0, 0, 120 * time.Second},
		{"configured", 5 * time.Second, 0, 5 * time.Second},
		{"earlier-parent", 0, 3 * time.Second, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := New(Config{BaseURL: "https://fixture.invalid", APIKey: "synthetic-admin-key", Timeout: tc.configuredLimit})
			client.http.Transport = deadlineProbeTransport{t: t, want: tc.want}
			ctx := context.Background()
			if tc.parentLimit > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parentLimit)
				defer cancel()
			}
			if _, err := client.Groups(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
