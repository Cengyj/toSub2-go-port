package legacyruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
	"tosub2-go-port/internal/transport"
)

var errChallengeBudget = errors.New("challenge solver time budget exhausted")

// Keep diagnostics to fixed stage names, flags and counters. Challenge scripts,
// frame URLs, cookies and remote exception text can contain authentication data.
type challengeProgress struct {
	stage                                         string
	frame, child, init, extraParams, execute      bool
	requests, lastHTTP, parentErrors, childErrors int
}

func (p *challengeProgress) waiting() {
	switch {
	case p.child:
		p.stage = "child_execution"
	case p.frame:
		p.stage = "frame_start"
	default:
		p.stage = "parent_execution"
	}
}

func (p *challengeProgress) failure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	reason := "verification failed"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "request deadline exceeded"
		if context.Cause(ctx) == errChallengeBudget {
			reason = "solver time budget exhausted"
		} else if ctx.Err() != nil {
			reason = "operation deadline exceeded"
		}
	} else if errors.Is(err, context.Canceled) {
		reason = "operation canceled"
	}
	return fmt.Errorf("LEGACY_RUNTIME_CHALLENGE: %s [stage=%s frame=%t child=%t init=%t extraParams=%t execute=%t requests=%d last_http=%d parent_errors=%d child_errors=%d]: %w", reason, p.stage, p.frame, p.child, p.init, p.extraParams, p.execute, p.requests, p.lastHTTP, p.parentErrors, p.childErrors, err)
}

func (c *Client) Challenge(ctx context.Context, rawURL string) (err error) {
	if err = c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeoutCause(ctx, c.opts.SolverTimeout, errChallengeBudget)
	defer timeoutCancel()
	challengeURL, err := runtimeHTTPURL(rawURL)
	if err != nil {
		return err
	}
	body, ok := c.challenges[rawURL]
	if !ok || len(body) == 0 {
		return errors.New("LEGACY_RUNTIME_CHALLENGE: no matching challenge response in this session")
	}
	delete(c.challenges, rawURL)
	if !strings.Contains(string(body), "_cf_chl_opt") {
		return errors.New("LEGACY_RUNTIME_CHALLENGE: response does not contain a supported challenge")
	}
	progress := challengeProgress{stage: "parent_start"}
	defer func() { err = progress.failure(ctx, err) }()
	initial := clearanceValues(c.cookieJar(), rawURL)
	ident := identity(c.Profile())
	input := map[string]any{"url": rawURL, "html": string(body), "inlineScripts": inlineScripts(string(body)), "userAgent": c.UserAgent(), "browserIdentity": ident, "waitMs": remainingMillis(ctx), "performanceEntries": []any{performanceEntry(rawURL, len(body), 400)}}
	events := make(chan runtimeEvent, 64)
	parent, err := c.startRuntime(ctx, "parent", input, events)
	if err != nil {
		return err
	}
	var child *scriptRuntime
	defer func() { err = errors.Join(err, c.closeRuntimes(child, parent)) }()
	active := map[string]bool{"parent": true}
	parentMessages := []json.RawMessage{}
	parentMessageGraphs := []json.RawMessage{}
	frameURL := ""
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case event := <-events:
			if !active[event.source] {
				continue
			}
			if event.err != nil {
				return event.err
			}
			message := event.message
			switch message.Type {
			case "cookie":
				reply, err := c.submitRuntimeCookie(message)
				if err != nil {
					return err
				}
				if err = event.runtime.send(ctx, reply); err != nil {
					return err
				}
			case "failure":
				return runtimeFailure(message)
			case "request":
				progress.stage = event.source + "_request"
				progress.requests++
				page := rawURL
				if event.source == "child" {
					page = frameURL
				}
				reply, err := c.submitRuntimeRequest(ctx, page, message.Request, false)
				if err != nil {
					return err
				}
				if status, ok := reply["status"].(int); ok {
					progress.lastHTTP = status
				}
				progress.waiting()
				if err = event.runtime.send(ctx, reply); err != nil {
					return err
				}
			case "frame":
				if event.source != "parent" {
					return ErrProtocol
				}
				if _, err = runtimeHTTPURL(message.URL); err != nil {
					return err
				}
				if frameURL != "" && frameURL != message.URL {
					return errors.New("LEGACY_RUNTIME_CHALLENGE: multiple challenge frames are unsupported")
				}
				frameURL = message.URL
				progress.frame = true
			case "frame-message":
				if event.source != "parent" {
					return ErrProtocol
				}
				kind := frameMessageEvent(message.Message.Data)
				switch kind {
				case "init":
					progress.init = true
				case "extraParams":
					progress.extraParams = true
				case "execute":
					progress.execute = true
				}
				if child == nil {
					parentMessages = append(parentMessages, message.Message.Data)
					parentMessageGraphs = append(parentMessageGraphs, message.Message.Graph)
				} else if active["child"] {
					if err = child.send(ctx, forwardedWindowMessage("parent-message", message.Message.Data, message.Message.Graph)); err != nil {
						return err
					}
				}
			case "parent-message":
				if event.source != "child" {
					return ErrProtocol
				}
				kind := frameMessageEvent(message.Message.Data)
				if kind != "init" && kind != "requestExtraParams" {
					if err = parent.send(ctx, forwardedWindowMessage("child-message", message.Message.Data, message.Message.Graph)); err != nil {
						return err
					}
				}
			case "final":
				count := len(message.Result.Errors) + len(message.Result.ReportedErrors)
				if event.source == "parent" {
					progress.parentErrors = max(progress.parentErrors, count)
				} else {
					progress.childErrors = max(progress.childErrors, count)
				}
				active[event.source] = false
			case "diagnostic":
				count := min(max(0, message.ErrorCount), 1000000)
				if event.source == "parent" {
					progress.parentErrors = max(progress.parentErrors, count)
				} else {
					progress.childErrors = max(progress.childErrors, count)
				}
			default:
				return ErrProtocol
			}
		}
		// A real iframe loads before it receives execute. Waiting for execute
		// before starting the child deadlocks widgets that first send ready.
		if child == nil && frameURL != "" {
			progress.stage = "frame_request"
			frameHeaders := c.navigationHeaders()
			frameHeaders.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
			frameHeaders.Set("Referer", rawURL)
			frameHeaders.Set("Sec-Fetch-Dest", "iframe")
			frameHeaders.Set("Sec-Fetch-Site", "cross-site")
			frameHeaders.Del("Sec-Fetch-User")
			frameHeaders.Del("Upgrade-Insecure-Requests")
			frame, err := c.runtimeGet(ctx, frameURL, frameHeaders, true, nil)
			if err != nil {
				return err
			}
			if frame.StatusCode != http.StatusOK {
				progress.lastHTTP = frame.StatusCode
				return errors.New("LEGACY_RUNTIME_CHALLENGE: challenge frame did not return HTTP 200")
			}
			progress.lastHTTP = frame.StatusCode
			progress.stage = "child_start"
			childInput := map[string]any{"url": frameURL, "parentUrl": rawURL, "html": string(frame.Body), "userAgent": c.UserAgent(), "browserIdentity": ident, "waitMs": remainingMillis(ctx), "performanceEntries": []any{performanceEntry(frameURL, len(frame.Body), 700)}, "parentMessages": parentMessages}
			childInput["parentMessageGraphs"] = parentMessageGraphs
			child, err = c.startRuntime(ctx, "child", childInput, events)
			if err != nil {
				return err
			}
			active["child"] = true
			progress.child = true
			parentMessages = nil
			parentMessageGraphs = nil
			progress.waiting()
		}
		fresh := false
		for value := range clearanceValues(c.cookieJar(), rawURL) {
			if !initial[value] {
				fresh = true
				break
			}
		}
		if fresh {
			progress.stage = "clearance_verification"
			verificationURL := challengeURL.Scheme + "://" + challengeURL.Host + "/"
			verified, err := c.runtimeGet(ctx, verificationURL, c.navigationHeaders(), false, nil)
			if err != nil {
				return err
			}
			if !verifiedChallengeResponse(verified, verificationURL) {
				progress.lastHTTP = verified.StatusCode
				return errors.New("LEGACY_RUNTIME_CHALLENGE: new clearance did not pass page verification")
			}
			return nil
		}
		if !active["parent"] && !active["child"] {
			return errors.New("LEGACY_RUNTIME_CHALLENGE: dynamic runtimes completed without a new clearance cookie")
		}
	}
}

// Structured-clone graphs are opaque to the HTTP owner. It retains the plain
// event metadata for challenge progress while forwarding the host's graph
// without decoding its objects or changing its data frames.
func forwardedWindowMessage(kind string, data, graph json.RawMessage) map[string]any {
	command := map[string]any{"type": kind, "data": data}
	if len(graph) != 0 {
		command["graph"] = graph
	}
	return command
}

func frameMessageEvent(raw json.RawMessage) string {
	var value struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(raw, &value)
	return value.Event
}

func remainingMillis(ctx context.Context) int64 {
	if deadline, ok := ctx.Deadline(); ok {
		return max(int64(1), time.Until(deadline).Milliseconds())
	}
	return 120000
}

func performanceEntry(rawURL string, size int, duration float64) map[string]any {
	return map[string]any{"name": rawURL, "entryType": "navigation", "startTime": 0, "duration": duration, "requestStart": duration * .08, "responseStart": duration * .75, "responseEnd": duration, "transferSize": size, "encodedBodySize": size, "decodedBodySize": size}
}

func inlineScripts(body string) []string {
	scripts := []string{}
	tokenizer := html.NewTokenizer(strings.NewReader(body))
	inside, src := false, false
	var text strings.Builder
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return scripts
			}
			return scripts
		case html.StartTagToken:
			token := tokenizer.Token()
			if strings.EqualFold(token.Data, "script") {
				inside = true
				src = false
				text.Reset()
				for _, attr := range token.Attr {
					if strings.EqualFold(attr.Key, "src") && attr.Val != "" {
						src = true
					}
				}
			}
		case html.TextToken:
			if inside {
				text.Write(tokenizer.Text())
			}
		case html.EndTagToken:
			if inside && strings.EqualFold(tokenizer.Token().Data, "script") {
				if !src && strings.TrimSpace(text.String()) != "" {
					scripts = append(scripts, text.String())
				}
				inside = false
			}
		}
	}
}

func clearanceValues(jar *transport.CookieJar, rawURL string) map[string]bool {
	values := map[string]bool{}
	for _, part := range strings.Split(jar.HeaderFor(rawURL), ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name == "cf_clearance" && value != "" {
			values[value] = true
		}
	}
	return values
}

func (c *Client) navigationHeaders() http.Header {
	h := http.Header{"User-Agent": []string{c.UserAgent()}, "Accept": []string{"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"}, "Accept-Language": []string{c.AcceptLanguage()}, "Sec-Fetch-Dest": []string{"document"}, "Sec-Fetch-Mode": []string{"navigate"}, "Sec-Fetch-Site": []string{"none"}, "Sec-Fetch-User": []string{"?1"}, "Upgrade-Insecure-Requests": []string{"1"}}
	ident := identity(c.Profile())
	for key, name := range map[string]string{"secChUa": "Sec-Ch-Ua", "secChUaMobile": "Sec-Ch-Ua-Mobile", "secChUaPlatform": "Sec-Ch-Ua-Platform"} {
		if value, ok := ident[key].(string); ok && value != "" {
			h.Set(name, value)
		}
	}
	return curlRuntimeDefaults(h)
}

func verifiedChallengeResponse(response *transport.Response, requestURL string) bool {
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return false
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Cf-Mitigated")+response.Header.Get("X-Cf-Mitigated")), "challenge") {
		return false
	}
	body := strings.ToLower(string(response.Body))
	for _, marker := range []string{"just a moment", "_cf_chl_opt", "cdn-cgi/challenge", "challenge-platform", "cf-challenge"} {
		if strings.Contains(body, marker) {
			return false
		}
	}
	if response.StatusCode >= 300 {
		base, err := url.Parse(requestURL)
		if err != nil {
			return false
		}
		target, err := url.Parse(response.Header.Get("Location"))
		if err != nil || response.Header.Get("Location") == "" {
			return false
		}
		target = base.ResolveReference(target)
		if !sameOrigin(base, target) {
			return false
		}
		for _, marker := range []string{"cdn-cgi", "challenge", "captcha", "security-check", "verify"} {
			if strings.Contains(strings.ToLower(target.Path+"?"+target.RawQuery), marker) {
				return false
			}
		}
	}
	return true
}
