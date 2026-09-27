package protocol

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestFollowStripsOriginBoundSecretsAcrossRedirects(t *testing.T) {
	for _, destination := range []string{"https://other.example/next", "https://auth.example:444/next", "http://auth.example/next"} {
		t.Run(destination, func(t *testing.T) {
			calls := 0
			initial := http.Header{"authorization": {"Bearer fixture-access"}, "Cookie": {"private=fixture-cookie"}, "openai-sentinel-token": {"fixture-token"}, "Openai-Sentinel-So-Token": {"fixture-observer"}, "Oai-Device-Id": {"fixture-device"}, "X-Trace": {"fixture-public"}}
			c := NewClient(doerFunc(func(_ context.Context, req transport.Request) (*transport.Response, error) {
				calls++
				if calls == 1 {
					r := response(302, "")
					r.Header.Set("Location", "/same-origin")
					return r, nil
				}
				if calls == 2 {
					if req.Headers.Get("Authorization") == "" || req.Headers.Get("Cookie") == "" || req.Headers.Get("Openai-Sentinel-Token") == "" {
						t.Error("same-origin credentials unexpectedly removed")
					}
					r := response(302, "")
					r.Header.Set("Location", destination)
					return r, nil
				}
				for _, key := range []string{"Authorization", "Cookie", "Openai-Sentinel-Token", "Openai-Sentinel-SO-Token", "Oai-Device-Id", "Origin"} {
					if req.Headers.Get(key) != "" {
						t.Errorf("cross-origin redirect leaked %s", key)
					}
				}
				if req.Headers.Get("X-Trace") != "fixture-public" {
					t.Error("non-sensitive header lost")
				}
				if strings.Contains(req.Headers.Get("Referer"), "private-query") || (strings.HasPrefix(destination, "http:") && req.Headers.Get("Referer") != "") {
					t.Error("sensitive or downgraded referer leaked")
				}
				return response(200, "ok"), nil
			}), nil, "")
			if _, err := c.Follow(context.Background(), "https://auth.example/start", RequestOptions{Headers: initial, Origin: "https://auth.example", Referer: "https://auth.example/?code=private-query"}); err != nil {
				t.Fatal(err)
			}
			if calls != 3 || initial["authorization"][0] == "" {
				t.Fatal("redirect calls or original caller headers changed")
			}
		})
	}
}

func TestSameOriginTreatsDefaultPortsAsEquivalent(t *testing.T) {
	a, _ := url.Parse("https://auth.example/a")
	b, _ := url.Parse("https://AUTH.example:443/b")
	c, _ := url.Parse("http://auth.example:443/b")
	if !sameOrigin(a, b) || sameOrigin(a, c) {
		t.Fatal("origin comparison differs from browser scheme/host/port rules")
	}
}
