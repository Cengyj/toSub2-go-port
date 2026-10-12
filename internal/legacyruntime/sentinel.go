package legacyruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"tosub2-go-port/internal/transport"
)

const sentinelLoaderURL = "https://sentinel.openai.com/backend-api/sentinel/sdk.js"

var sentinelSDKPattern = regexp.MustCompile(`(?:https?://[^\s"'<>]+)?/sentinel/[^\s"'<>]+/sdk\.js`)

func (c *Client) Sentinel(ctx context.Context, authBase, device, flow, pageURL string) (headers http.Header, err error) {
	if err = c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, c.opts.SolverTimeout)
	defer timeoutCancel()
	if pageURL == "" {
		pageURL = strings.TrimRight(authBase, "/") + "/log-in"
	}
	page, err := runtimeHTTPURL(pageURL)
	if err != nil {
		return nil, err
	}
	if device == "" {
		device = c.cookieJar().Value("oai-did", pageURL)
	}
	if device == "" {
		var id [16]byte
		if _, err = rand.Read(id[:]); err != nil {
			return nil, errors.New("SENTINEL_DEVICE_MISSING: cannot create a device identifier")
		}
		id[6] = (id[6] & 15) | 64
		id[8] = (id[8] & 63) | 128
		encoded := hex.EncodeToString(id[:])
		device = encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
	}
	domain := page.Hostname()
	hostOnly := true
	if domain == "openai.com" || strings.HasSuffix(domain, ".openai.com") {
		domain = "openai.com"
		hostOnly = false
	}
	c.cookieJar().Set(transport.Cookie{Name: "oai-did", Value: device, Domain: domain, Path: "/", Secure: page.Scheme == "https", HostOnly: hostOnly})
	loaderURL := c.opts.SDKURL
	if loaderURL == "" {
		loaderURL = sentinelLoaderURL
	}
	loaderOrigin, err := runtimeHTTPURL(loaderURL)
	if err != nil {
		return nil, err
	}
	loaderHeaders := http.Header{"Accept": []string{"*/*"}, "Referer": []string{pageURL}, "Sec-Fetch-Dest": []string{"script"}, "Sec-Fetch-Mode": []string{"no-cors"}, "Sec-Fetch-Site": []string{"cross-site"}}
	loader, err := c.runtimeGet(ctx, loaderURL, loaderHeaders, true, nil)
	if err != nil {
		return nil, err
	}
	if loader.StatusCode != http.StatusOK {
		return nil, errors.New("SENTINEL_LOADER_HTTP: loader did not return HTTP 200")
	}
	match := sentinelSDKPattern.FindString(string(loader.Body))
	if match == "" {
		return nil, errors.New("SENTINEL_SDK_MISSING: loader did not expose a versioned SDK URL")
	}
	sdkRef, err := url.Parse(match)
	if err != nil {
		return nil, ErrProtocol
	}
	sdkURL := loaderOrigin.ResolveReference(sdkRef)
	if !sameOrigin(loaderOrigin, sdkURL) {
		return nil, errors.New("SENTINEL_SDK_ORIGIN: SDK must use the configured loader origin")
	}
	sdkHeaders := loaderHeaders.Clone()
	sdkHeaders.Set("Referer", loaderURL)
	sdkHeaders.Set("Sec-Fetch-Site", "same-origin")
	sdk, err := c.runtimeGet(ctx, sdkURL.String(), sdkHeaders, true, loaderOrigin)
	if err != nil {
		return nil, err
	}
	if sdk.StatusCode != http.StatusOK {
		return nil, errors.New("SENTINEL_SDK_HTTP: SDK did not return HTTP 200")
	}
	ident := identity(c.Profile())
	input := map[string]any{"url": pageURL, "html": "<!doctype html><html><head></head><body></body></html>", "userAgent": c.UserAgent(), "platform": ident["platform"], "language": ident["locale"], "languages": ident["languages"], "cookies": map[string]string{"oai-did": device}, "screenWidth": 1920, "screenHeight": 1080, "loaderUrl": loaderURL, "loaderSource": string(loader.Body), "sdkUrl": sdkURL.String(), "sdkSource": string(sdk.Body)}
	events := make(chan runtimeEvent, 32)
	runtime, err := c.startRuntime(ctx, "sentinel", input, events)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, c.closeRuntimes(runtime))
		if err != nil {
			headers = nil
		}
	}()
	token, err := c.sentinelCall(ctx, runtime, events, pageURL, "token", flow)
	if err != nil {
		return nil, err
	}
	if err = validateSentinelToken(token, []string{"p", "t", "c", "id", "flow"}); err != nil {
		return nil, err
	}
	so, err := c.sentinelCall(ctx, runtime, events, pageURL, "sessionObserverToken", flow)
	if err != nil {
		return nil, err
	}
	if so != "" {
		if err = validateSentinelToken(so, []string{"so", "c", "id", "flow"}); err != nil {
			return nil, err
		}
	}
	headers = make(http.Header)
	headers.Set("Openai-Sentinel-Token", token)
	if so != "" {
		headers.Set("Openai-Sentinel-So-Token", so)
	}
	return headers, nil
}

func validateSentinelToken(token string, required []string) error {
	var parsed map[string]json.RawMessage
	if token == "" || json.Unmarshal([]byte(token), &parsed) != nil || parsed == nil {
		return errors.New("SENTINEL_TOKEN_INVALID: dynamic SDK returned an invalid token")
	}
	for _, key := range required {
		if _, ok := parsed[key]; !ok {
			return errors.New("SENTINEL_TOKEN_INVALID: dynamic SDK token is incomplete")
		}
	}
	if value, ok := parsed["e"]; ok && string(value) != "null" && string(value) != "\"\"" && string(value) != "false" {
		return errors.New("SENTINEL_TOKEN_INVALID: dynamic SDK reported a token error")
	}
	return nil
}

func (c *Client) sentinelCall(ctx context.Context, runtime *scriptRuntime, events <-chan runtimeEvent, pageURL, method, flow string) (string, error) {
	if err := runtime.send(ctx, map[string]any{"type": "call", "method": method, "flow": flow}); err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case event := <-events:
			if event.err != nil {
				return "", event.err
			}
			switch event.message.Type {
			case "cookie":
				reply, err := c.submitRuntimeCookie(event.message)
				if err != nil {
					return "", err
				}
				if err = runtime.send(ctx, reply); err != nil {
					return "", err
				}
			case "failure":
				return "", runtimeFailure(event.message)
			case "request":
				page := event.message.Request.SourceURL
				if page == "" {
					page = pageURL
				}
				reply, err := c.submitRuntimeRequest(ctx, page, event.message.Request, true)
				if err != nil {
					return "", err
				}
				if err = runtime.send(ctx, reply); err != nil {
					return "", err
				}
			case "result":
				if event.message.Method != method || event.message.Flow != flow {
					return "", ErrProtocol
				}
				if len(event.message.Errors) > 0 {
					return "", errors.New("SENTINEL_RUNTIME_FAILED: dynamic SDK reported a runtime error")
				}
				if event.message.Value == nil || *event.message.Value == "" {
					if method == "sessionObserverToken" {
						return "", nil
					}
					return "", errors.New("SENTINEL_TOKEN_MISSING: dynamic SDK returned no token")
				}
				return *event.message.Value, nil
			case "frame": // The Sentinel runtime owns its frame in this process.
			default:
				return "", ErrProtocol
			}
		}
	}
}
