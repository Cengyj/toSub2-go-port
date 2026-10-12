package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/legacyruntime"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

const legacyProfilePrefix = "tls-client:"

// Validate a saved session before sending account requests. Its HTTP identity
// must match the remaining Go authentication runtime.
func validateGoAuthenticationCheckpoint(checkpoint *protocol.Checkpoint) error {
	if checkpoint == nil {
		return nil
	}
	binding, present, err := protocol.CheckpointTransportSession(*checkpoint)
	if err != nil {
		return err
	}
	if !present || binding.BrowserBound || !strings.HasPrefix(binding.TLSProfile, legacyProfilePrefix) {
		return errors.New("CHECKPOINT_SESSION_MISMATCH: checkpoint does not use the current Go authentication runtime; explicitly re-login")
	}
	return nil
}

func newLegacyAuthentication(ctx context.Context, cfg config.Config, proxy string, checkpoint *protocol.Checkpoint, jar *transport.CookieJar, retryCallbacks ...func(transport.ReadRetryEvent)) (*legacyruntime.Client, error) {
	profile := cfg.AuthTLSProfile
	if profile == "" {
		profile = "chrome146"
	}
	if checkpoint != nil {
		if err := validateGoAuthenticationCheckpoint(checkpoint); err != nil {
			return nil, err
		}
		binding, _, _ := protocol.CheckpointTransportSession(*checkpoint)
		profile = strings.TrimPrefix(binding.TLSProfile, legacyProfilePrefix)
	}
	var onRetry func(transport.ReadRetryEvent)
	if len(retryCallbacks) > 0 {
		onRetry = retryCallbacks[0]
	}
	client, err := legacyruntime.New(ctx, legacyruntime.Options{
		SDKURL:   cfg.SentinelSDKURL,
		ProxyURL: proxy, Profile: profile, Timeout: time.Duration(cfg.RequestTimeoutMS) * time.Millisecond,
		SolverTimeout:     120 * time.Second,
		ConnectionRetries: cfg.SameProxyRetries,
		RetryDelay:        time.Duration(cfg.SameProxyRetryDelayMS) * time.Millisecond,
		OnRetry:           onRetry,
	})
	if err != nil {
		return nil, err
	}
	if checkpoint != nil {
		if err := protocol.ValidateTransportSession(*checkpoint, legacyProfilePrefix+client.Profile(), client.UserAgent(), proxy); err != nil {
			return nil, errors.Join(err, client.Close())
		}
		jar.Merge(checkpoint.Cookies)
	}
	client.SetJar(jar)
	return client, nil
}

// Only a fresh session may try another proxy after a connection failure. An
// HTTP response (including a challenge or rejection) commits to that session.
func prepareLegacyAuthentication(ctx context.Context, cfg config.Config, selected string, retryCallbacks ...func(transport.ReadRetryEvent)) (*legacyruntime.Client, *transport.CookieJar, string, error) {
	if cfg.SameProxyRetries < 0 || cfg.SameProxyRetries > 20 {
		return nil, nil, "", errors.New("connection retry count must be between 0 and 20")
	}
	var onRetry func(transport.ReadRetryEvent)
	if len(retryCallbacks) > 0 {
		onRetry = retryCallbacks[0]
	}
	templates := []string{selected}
	if selected == "" && len(cfg.ProxyURLs) > 0 {
		templates = cfg.ProxyURLs
	}
	budget, err := proxyConnectionAttemptBudget(cfg, templates)
	if err != nil {
		return nil, nil, "", err
	}
	var last error
	failures := 0
	maxRequests := legacyPreflightRequestBudget(cfg, templates, budget)
	for attempt := 0; attempt < budget; attempt++ {
		template := templates[attempt%len(templates)]
		proxy := transport.NewProxySession(template).Rotate()
		if attempt >= len(templates) && proxy == template {
			break
		}
		for same := 0; same <= max(0, cfg.SameProxyRetries); same++ {
			if failures >= maxProxyConnectionFailures {
				break
			}
			if err = ctx.Err(); err != nil {
				return nil, nil, "", err
			}
			if failures > 0 && onRetry != nil {
				onRetry(transport.ReadRetryEvent{Retry: failures, MaxRetries: maxRequests - 1, Delay: proxyConnectionRetryDelay(cfg, failures), Err: last})
			}
			if err = waitProxyConnectionRetry(ctx, cfg, failures); err != nil {
				return nil, nil, "", err
			}
			jar := transport.NewCookieJar()
			client, err := newLegacyAuthentication(ctx, cfg, proxy, nil, jar, onRetry)
			if err != nil {
				return nil, nil, "", err
			}
			_, err = client.Do(ctx, transport.Request{Method: http.MethodGet, URL: strings.TrimRight(cfg.ChatGPTBase, "/") + "/", Timeout: time.Duration(cfg.RequestTimeoutMS) * time.Millisecond, SingleAttempt: true})
			if err == nil {
				return client, jar, proxy, nil
			}
			last = err
			if cleanupErr := client.Close(); cleanupErr != nil {
				return nil, nil, "", errors.Join(err, cleanupErr)
			}
			if ctx.Err() != nil {
				return nil, nil, "", ctx.Err()
			}
			if !transport.IsRetryableReadError(err) {
				return nil, nil, "", err
			}
			failures++
		}
	}
	if last == nil {
		last = errors.New("legacy authentication proxy attempt budget exhausted")
	}
	if failures > 1 {
		if onRetry != nil {
			onRetry(transport.ReadRetryEvent{Retry: failures - 1, MaxRetries: failures - 1, Err: last, Exhausted: true})
		}
		last = transport.ReadRetriesExhausted(last, failures-1)
	}
	return nil, nil, "", last
}

// Fixed endpoints are not rotated. Count the actual preflight request limit so
// its logs report the shared budget rather than multiplying nested retries.
func legacyPreflightRequestBudget(cfg config.Config, templates []string, identities int) int {
	requests := 0
	for n := 0; n < identities; n++ {
		template := templates[n%len(templates)]
		if n >= len(templates) && !transport.ProxySupportsSessionRotation(template) {
			break
		}
		requests += cfg.SameProxyRetries + 1
		if requests >= maxProxyConnectionFailures {
			return maxProxyConnectionFailures
		}
	}
	return requests
}
