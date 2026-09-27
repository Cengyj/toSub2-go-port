package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestChromiumInterruptedBodySynchronizesAcceptedCookiesForCheckpoint(t *testing.T) {
	s, ctx := integrationSession(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resume" {
			fmt.Fprint(w, r.Header.Get("Cookie"))
			return
		}
		w.Header().Add("Set-Cookie", "old_session=; Path=/; Max-Age=0; HttpOnly")
		w.Header().Add("Set-Cookie", "new_session=synthetic; Path=/; HttpOnly")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	jar := transport.NewCookieJar()
	jar.SetFromResponse(server.URL, http.Header{"Set-Cookie": {"old_session=synthetic-old; Path=/; HttpOnly"}})
	c := NewTransport(s, jar)
	_, err := c.Do(ctx, transport.Request{URL: server.URL + "/slow", Timeout: 250 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected interrupted response body: %v", err)
	}
	if got := jar.HeaderFor(server.URL); got != "new_session=synthetic" {
		cookies, inspectErr := s.CookiesContext(ctx)
		var names []string
		for _, cookie := range cookies {
			if name, ok := cookie["name"].(string); ok {
				names = append(names, name)
			}
		}
		t.Fatalf("checkpoint cookie jar lagged behind accepted response (browser cookie names=%v, inspect=%v)", names, inspectErr)
	}
	response, err := c.Do(ctx, transport.Request{URL: server.URL + "/resume", Timeout: time.Second})
	if err != nil || response.Text() != "new_session=synthetic" {
		t.Fatalf("resumed browser request sent wrong session: %v", err)
	}
}
