package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func TestAuthenticationRuntimePreservesGoCheckpoint(t *testing.T) {
	checkpoint := &protocol.Checkpoint{Stage: "phone_required"}
	protocol.BindTransportSession(checkpoint, legacyProfilePrefix+"chrome_146", "fixture-UA", "", false)
	for _, cp := range []*protocol.Checkpoint{nil, checkpoint} {
		if err := validateGoAuthenticationCheckpoint(cp); err != nil {
			t.Fatal("Go checkpoint rejected", err)
		}
	}

}

func TestGoAuthenticationPreflightTimeoutRetryLogsAndBudget(t *testing.T) {
	for _, recoverAfter := range []int32{2, 99} {
		scenario := "exhausted"
		if recoverAfter == 2 {
			scenario = "recovers"
		}
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) <= recoverAfter {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			cfg := config.Config{ChatGPTBase: server.URL, AuthTLSProfile: "chrome146", RequestTimeoutMS: 30, SameProxyRetries: 3, SameProxyRetryDelaySet: true}
			var events []transport.ReadRetryEvent
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			client, _, _, err := prepareLegacyAuthentication(ctx, cfg, "", func(e transport.ReadRetryEvent) { events = append(events, e) })
			if recoverAfter == 2 {
				if err != nil || calls.Load() != 3 || client == nil || len(events) != 2 {
					t.Fatalf("preflight failed before retry recovery: calls=%d events=%v err=%v", calls.Load(), events, err)
				}
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || calls.Load() != 4 || len(events) != 4 || !events[3].Exhausted || !strings.Contains(err.Error(), "3") {
				t.Fatalf("preflight exceeded or hid retry budget: calls=%d events=%v err=%v", calls.Load(), events, err)
			}
			for i, event := range events {
				if event.MaxRetries != 3 || (!event.Exhausted && event.Retry != i+1) || !strings.Contains(event.Message(), "重试") {
					t.Fatalf("preflight retry log incorrect: %+v", event)
				}
			}
		})
	}
}

func TestAuthenticationRuntimeRejectsDamagedOrForeignBindings(t *testing.T) {
	if err := validateGoAuthenticationCheckpoint(&protocol.Checkpoint{Stage: "web_mfa"}); err == nil {
		t.Fatal("checkpoint without Go identity accepted")
	}
	for _, binding := range []any{
		map[string]any{"tls_profile": "curl_cffi:chrome146"},
		map[string]any{"tls_profile": "curl_cffi:chrome146", "user_agent": "fixture", "proxy_sha256": "bad-digest"},
		"unreadable-binding",
	} {
		checkpoint := &protocol.Checkpoint{Stage: "phone_required", Extra: map[string]any{"transport_session": binding}}
		if err := validateGoAuthenticationCheckpoint(checkpoint); err == nil {
			t.Fatal("damaged binding accepted")
		}
	}
	for _, tc := range []struct {
		profile string
		browser bool
		want    string
	}{
		{"chrome_133", false, "CHECKPOINT_SESSION_MISMATCH"},
		{"curl_cffi:chrome146", false, "CHECKPOINT_SESSION_MISMATCH"},
		{"chromium-native", true, "CHECKPOINT_SESSION_MISMATCH"},
	} {
		checkpoint := &protocol.Checkpoint{Stage: "web_mfa"}
		protocol.BindTransportSession(checkpoint, tc.profile, "fixture-UA", "", tc.browser)
		if err := validateGoAuthenticationCheckpoint(checkpoint); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatal("foreign identity accepted", err)
		}
		if _, err := newLegacyAuthentication(context.Background(), config.Config{}, "", checkpoint, transport.NewCookieJar()); err == nil || !strings.Contains(err.Error(), "CHECKPOINT_SESSION_MISMATCH") {
			t.Fatal("foreign checkpoint reached runtime startup", err)
		}
	}
}

func TestGoCheckpointRetainsProfileAndCookies(t *testing.T) {
	profile := transport.ProfileFor("chrome_146")
	checkpoint := &protocol.Checkpoint{Stage: "web_mfa", Cookies: []transport.Cookie{{Name: "fixture-session", Value: "fixture", Domain: "auth.example.test", Path: "/", HostOnly: true}}}
	protocol.BindTransportSession(checkpoint, legacyProfilePrefix+"chrome_146", profile.UserAgent, "", false)
	jar := transport.NewCookieJar()
	client, err := newLegacyAuthentication(context.Background(), config.Config{}, "", checkpoint, jar)
	if err != nil {
		t.Fatal(err)
	}
	if client.Profile() != "chrome_146" || client.UserAgent() != profile.UserAgent || jar.HeaderFor("https://auth.example.test/") != "fixture-session=fixture" {
		t.Fatal("Go HTTP identity/cookies changed")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}
