package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

var errCheckpointSessionUnverified = errors.New("CHECKPOINT_SESSION_UNVERIFIED: this pending authentication checkpoint has no matching Chromium identity and cannot be verified without replaying account operations; the original checkpoint is preserved; explicitly choose re-login to start a new authorized session")

func validateAuthenticationCheckpoint(c protocol.Checkpoint, cfg config.Config, email string) error {
	// Flow.Restore performs the authoritative schema/account/origin checks in
	// an isolated jar. Run them before using any checkpoint cookie on network.
	probe := protocol.NewFlow(nil, protocol.FlowOptions{ChatGPTBase: cfg.ChatGPTBase, AuthBase: cfg.AuthBase, Email: email})
	return probe.Restore(c)
}

// A received code is bound by PKCE/state, not by browser cookies. It can be
// exchanged through Go HTTP without migrating the checkpoint's web session.
func cookieIndependentCallback(c *protocol.Checkpoint, operation string) bool {
	if c == nil || c.Stage != "callback_ready" {
		return false
	}
	switch operation {
	case "", "login", "full", "resume":
		return true
	}
	return false
}

// Existing native-Chromium checkpoints remain resumable even when their old
// bootstrap used Go. We preserve that recorded identity, without claiming its
// earliest request was made by Chromium. Only authenticated web sessions have
// an available read-only, account-specific migration proof.
func checkpointBrowserPolicy(c *protocol.Checkpoint) (revalidate bool, err error) {
	if c == nil {
		return false, nil
	}
	binding, present, err := protocol.CheckpointTransportSession(*c)
	if err != nil {
		return false, err
	}
	if present && binding.BrowserBound {
		if binding.TLSProfile != "chromium-native" {
			return false, errCheckpointSessionUnverified
		}
		return false, nil
	}
	if c.Stage == "email_verified" {
		return true, nil
	}
	return false, errCheckpointSessionUnverified
}

func authenticationDevice(c *protocol.Checkpoint, jar *transport.CookieJar, cfg config.Config) (string, error) {
	device := ""
	if c != nil {
		device, _ = c.Web["deviceId"].(string)
		for _, cookie := range c.Cookies {
			if cookie.Name != "oai-did" {
				continue
			}
			if device != "" && cookie.Value != device {
				return "", errors.New("CHECKPOINT_SESSION_MISMATCH: saved device identifiers disagree; explicitly re-login")
			}
			device = cookie.Value
		}
		if device == "" {
			return "", errors.New("CHECKPOINT_SESSION_UNVERIFIED: saved browser device identifier is missing; checkpoint preserved; explicitly re-login")
		}
	} else {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		raw[6] = (raw[6] & 0x0f) | 0x40
		raw[8] = (raw[8] & 0x3f) | 0x80
		device = fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
	}
	for _, base := range []string{cfg.ChatGPTBase, cfg.AuthBase} {
		u, err := url.Parse(base)
		if err != nil || u.Hostname() == "" {
			return "", errors.New("invalid authentication origin")
		}
		if existing := jar.Value("oai-did", base+"/"); existing != "" && existing != device {
			return "", errors.New("CHECKPOINT_SESSION_MISMATCH: saved cookie device differs from browser identity; explicitly re-login")
		}
		jar.Set(transport.Cookie{Name: "oai-did", Value: device, Domain: u.Hostname(), Path: "/", HostOnly: true, Secure: u.Scheme == "https", SameSite: "Lax"})
	}
	return device, nil
}

// prepareBrowserAuthentication probes only a fresh, empty authentication
// session. The first HTTP request, its device cookie, subsequent protocol
// requests and the actual SDK therefore all originate from the same Chrome.
// A received HTTP response (including a challenge) fixes the identity; proxy
// rotation is limited to transport failures before a response was obtained.
func prepareBrowserAuthentication(ctx context.Context, cfg config.Config, pool *browser.Manager, selected, device string) (*browser.Session, *browser.Transport, *transport.CookieJar, string, error) {
	templates := []string{selected}
	if selected == "" && len(cfg.ProxyURLs) > 0 {
		templates = cfg.ProxyURLs
	}
	budget, err := proxyConnectionAttemptBudget(cfg, templates)
	if err != nil {
		return nil, nil, nil, "", err
	}
	var last error
	failures := 0
	for attempt := 0; attempt < budget; attempt++ {
		template := templates[attempt%len(templates)]
		proxy := transport.NewProxySession(template).Rotate()
		if attempt >= len(templates) && proxy == template {
			break
		}
		session, err := pool.StartWait(ctx, proxy, "", "chromium-native", device)
		if err != nil {
			return nil, nil, nil, "", err
		}
		jar := transport.NewCookieJar()
		for _, base := range []string{cfg.ChatGPTBase, cfg.AuthBase} {
			u, e := url.Parse(base)
			if e != nil || u.Hostname() == "" {
				return nil, nil, nil, "", errors.Join(errors.New("invalid authentication origin"), pool.Stop(session))
			}
			jar.Set(transport.Cookie{Name: "oai-did", Value: device, Domain: u.Hostname(), Path: "/", HostOnly: true, Secure: u.Scheme == "https", SameSite: "Lax"})
		}
		bound := browser.NewTransport(session, jar)
		for same := 0; same <= max(0, cfg.SameProxyRetries); same++ {
			if failures >= maxProxyConnectionFailures {
				return nil, nil, nil, "", errors.Join(last, pool.Stop(session))
			}
			if err = waitProxyConnectionRetry(ctx, cfg, failures); err != nil {
				return nil, nil, nil, "", errors.Join(err, pool.Stop(session))
			}
			_, last = bound.Do(ctx, transport.Request{Method: http.MethodGet, URL: cfg.ChatGPTBase + "/", Timeout: time.Duration(maxRequestTimeout(cfg.RequestTimeoutMS)) * time.Millisecond})
			if last == nil {
				return session, bound, jar, proxy, nil
			}
			if ctx.Err() != nil || errors.Is(last, browser.ErrResponseStarted) || (!errors.Is(last, browser.ErrNetworkRequest) && !errors.Is(last, context.DeadlineExceeded)) {
				return nil, nil, nil, "", errors.Join(last, pool.Stop(session))
			}
			failures++
		}
		if err = pool.Stop(session); err != nil {
			return nil, nil, nil, "", errors.Join(last, err)
		}
	}
	if last == nil {
		last = errors.New("browser proxy connectivity attempts exhausted")
	}
	return nil, nil, nil, "", last
}

// The GET is made by the bound browser; a copied cookie or a generic HTML 200
// is not accepted as migration evidence. Redirects, empty/expired sessions,
// another account and challenge failures leave the checkpoint unchanged.
func revalidateLegacyWebSession(ctx context.Context, pc *protocol.Client, c protocol.Checkpoint, cfg config.Config) error {
	var session struct {
		AccessToken string `json:"accessToken"`
		Expires     string `json:"expires"`
		User        struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if _, err := pc.JSON(ctx, http.MethodGet, cfg.ChatGPTBase+"/api/auth/session", nil, protocol.RequestOptions{Referer: cfg.ChatGPTBase + "/"}, &session); err != nil {
		return errors.Join(errors.New("CHECKPOINT_SESSION_UNVERIFIED: read-only session verification failed; checkpoint preserved; explicitly re-login if the legacy session cannot be verified"), err)
	}
	expires, err := time.Parse(time.RFC3339Nano, session.Expires)
	if err != nil || !expires.After(time.Now()) || session.AccessToken == "" || !strings.EqualFold(strings.TrimSpace(session.User.Email), strings.TrimSpace(c.Email)) {
		return errors.New("CHECKPOINT_SESSION_UNVERIFIED: read-only response did not prove a live session for the checkpoint account; checkpoint preserved; explicitly re-login")
	}
	return nil
}
