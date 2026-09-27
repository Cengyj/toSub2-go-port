package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/transport"
)

// prepareFingerprint rotates only between fresh connectivity attempts. Security
// challenges keep their selected proxy/profile and move to the real browser.
// This runs before login or checkpoint state exists, so no tokens or cookies
// are copied between proxy identities.
func prepareFingerprint(ctx context.Context, cfg config.Config, selected string) (*transport.FingerprintClient, *transport.CookieJar, string, error) {
	templates := []string{selected}
	if selected == "" && len(cfg.ProxyURLs) > 0 {
		templates = cfg.ProxyURLs
	}
	max, budgetErr := proxyConnectionAttemptBudget(cfg, templates)
	if budgetErr != nil {
		return nil, nil, "", budgetErr
	}
	retries := cfg.SameProxyRetries
	if retries < 0 {
		retries = 0
	}
	var last error
	failures := 0
	for attempt := 0; attempt < max; attempt++ {
		template := templates[attempt%len(templates)]
		proxy := transport.NewProxySession(template).Rotate()
		// Fixed proxy endpoints get one identity attempt (plus same-session retries).
		if attempt >= len(templates) && proxy == template {
			break
		}
		jar := transport.NewCookieJar()
		for same := 0; same <= retries; same++ {
			if failures >= maxProxyConnectionFailures {
				return nil, nil, "", last
			}
			if err := waitProxyConnectionRetry(ctx, cfg, failures); err != nil {
				return nil, nil, "", err
			}
			if ctx.Err() != nil {
				return nil, nil, "", ctx.Err()
			}
			// Profile probing owns its 15s candidate / 300s total budget.
			// A normal request timeout must not cancel untried fallback profiles.
			c, e := transport.ProbeFingerprintClient(ctx, cfg.TLSProfile, proxy, cfg.ChatGPTBase+"/", jar)
			if e != nil && strings.Contains(e.Error(), "browser verification required") {
				c, e = transport.NewFingerprintClient(cfg.TLSProfile, proxy, jar)
			}
			if e == nil {
				return c, jar, proxy, nil
			}
			last = e
			failures++
		}
	}
	if last == nil {
		last = errors.New("proxy connectivity attempts exhausted")
	}
	return nil, nil, "", last
}

// prepareNative preserves the explicitly requested legacy diagnostic mode. It
// never runs as a fallback for a failed fingerprint profile. An HTTP response,
// including a security challenge, binds the chosen session; only connection
// failures may select another fresh proxy before authentication has started.
func prepareNative(ctx context.Context, cfg config.Config, selected string) (*transport.Client, *transport.CookieJar, string, error) {
	templates := []string{selected}
	if selected == "" && len(cfg.ProxyURLs) > 0 {
		templates = cfg.ProxyURLs
	}
	maxAttempts, budgetErr := proxyConnectionAttemptBudget(cfg, templates)
	if budgetErr != nil {
		return nil, nil, "", budgetErr
	}
	var last error
	failures := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		template := templates[attempt%len(templates)]
		proxy := transport.NewProxySession(template).Rotate()
		if attempt >= len(templates) && proxy == template {
			break
		}
		jar := transport.NewCookieJar()
		client, err := transport.NewNativeClient(proxy, jar)
		if err != nil {
			return nil, nil, "", err
		}
		for same := 0; same <= cfg.SameProxyRetries; same++ {
			if failures >= maxProxyConnectionFailures {
				client.Close()
				return nil, nil, "", last
			}
			if err := waitProxyConnectionRetry(ctx, cfg, failures); err != nil {
				client.Close()
				return nil, nil, "", err
			}
			if err := ctx.Err(); err != nil {
				client.Close()
				return nil, nil, "", err
			}
			_, last = client.Do(ctx, transport.Request{Method: http.MethodGet, URL: cfg.ChatGPTBase + "/", Timeout: time.Duration(maxRequestTimeout(cfg.RequestTimeoutMS)) * time.Millisecond})
			if last == nil {
				return client, jar, proxy, nil
			}
			failures++
		}
		client.Close()
	}
	if last == nil {
		last = errors.New("native proxy connectivity attempts exhausted")
	}
	return nil, nil, "", last
}

func maxRequestTimeout(ms int) int {
	if ms <= 0 {
		return 30000
	}
	return ms
}

const maxProxyConnectionFailures = 20

// A zero legacy session budget forbids rotating identities, but must not
// disable ordinary direct or fixed-proxy requests. Config{} retains one attempt.
func proxyConnectionAttemptBudget(cfg config.Config, templates []string) (int, error) {
	max := cfg.ProxyMaxAttempts
	if max == 0 && cfg.ProxyMaxAttemptsSet {
		if len(templates) > 0 && transport.ProxySupportsSessionRotation(templates[0]) {
			return 0, errors.New("PROXY_RISK_CONTROL: proxy session attempt budget exhausted")
		}
		return 1, nil
	}
	if max < 1 {
		max = 1
	}
	if len(templates) == 1 && templates[0] == "" {
		max = 1
	}
	return max, nil
}

func proxyConnectionRetryDelay(cfg config.Config, failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	base := cfg.ProxyConnectionRetryBaseMS
	if base <= 0 {
		base = 1000
	}
	if cfg.SameProxyRetryDelaySet {
		base = cfg.SameProxyRetryDelayMS
	}
	if base < 0 {
		base = 0
	}
	exponent := failures - 1
	if exponent > 4 {
		exponent = 4
	}
	// Compare before multiplying to avoid overflow for large configured values.
	const maxDelayMS = 15000
	multiplier := 1 << exponent
	if base >= (maxDelayMS+multiplier-1)/multiplier {
		return maxDelayMS * time.Millisecond
	}
	return time.Duration(base*multiplier) * time.Millisecond
}
func waitProxyConnectionRetry(ctx context.Context, cfg config.Config, failures int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay := proxyConnectionRetryDelay(cfg, failures)
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
