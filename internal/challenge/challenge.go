// Package challenge provides browser-backed Cloudflare/Turnstile/Sentinel hooks.
// Challenge solving is intentionally performed in the same Chromium session that
// owns the proxy, cookies, device and user-agent; tokens are never copied across
// sessions.
package challenge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"tosub2-go-port/internal/browser"
)

type Result struct {
	Solved bool
	// RetryReady means the document can be checked again through the same
	// BrowserTransport. It is not evidence that the protected request passed.
	RetryReady            bool
	Provider              string
	Token                 string
	SessionObserverToken  string
	SessionBound          bool
	SessionObserverStatus string
	Reason                string
}

type Solver interface{ Solve(string) (Result, error) }

type BrowserSolver struct {
	Session                browser.BrowserSession
	Timeout                time.Duration
	LoaderMaxAge           time.Duration
	RequireSessionObserver bool
	OnInteractionRequired  func(string)
}

func jsQuote(v string) string { b, _ := json.Marshal(v); return string(b) }

// Solve navigates the supplied BrowserSession and waits for a challenge token.
// The page itself executes Cloudflare/Turnstile/Sentinel JavaScript in Chromium;
// Go only observes the resulting token and session cookies.
func (s BrowserSolver) Solve(target string) (Result, error) {
	return s.SolveContext(context.Background(), target)
}

// SolveContext waits for a provider-issued response in the same real browser.
// A returned token is only evidence to retry the protected request through that
// session's BrowserTransport; it does not prove the upstream accepted clearance.
func (s BrowserSolver) SolveContext(parent context.Context, target string) (Result, error) {
	out := Result{Provider: "chromium"}
	if s.Session == nil {
		out.Reason = "real browser session required; configure BrowserSession/CDP"
		return out, errors.New(out.Reason)
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	oldClearance := s.clearanceSnapshot(ctx)
	if err := s.Session.Navigate(ctx, target); err != nil {
		diagnostic, known := s.currentDocumentDiagnostic(ctx)
		out.Reason = verificationDetail("browser navigation incomplete", diagnostic, known, false, false)
		if cause := verificationContextCause(ctx, err); cause != nil {
			return out, fmt.Errorf("%w: %s: %w", ErrVerificationPending, out.Reason, cause)
		}
		return out, errors.New(out.Reason)
	}
	return s.waitForChallenge(ctx, target, oldClearance)
}

func (s BrowserSolver) clearanceSnapshot(ctx context.Context) map[string]bool {
	oldClearance := map[string]bool{}
	if cookies, err := s.cookies(ctx); err == nil {
		for _, cookie := range cookies {
			if cookie["name"] == "cf_clearance" {
				if value, ok := cookie["value"].(string); ok {
					oldClearance[value] = true
				}
			}
		}
	}
	return oldClearance
}

// waitForChallenge keeps an already loaded provider challenge in its own page.
// It never clicks or substitutes challenge values and retains the bound session.
func (s BrowserSolver) waitForChallenge(ctx context.Context, target string, oldClearance map[string]bool) (Result, error) {
	out := Result{Provider: "chromium"}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	started := time.Now()
	prompted, observedChallenge := false, false
	var lastDiagnostic browser.NetworkDiagnostic
	diagnosticKnown, lastPending, pendingKnown := false, false, false
	for {
		if err := ctx.Err(); err != nil {
			out.Reason = verificationDetail("provider verification incomplete", lastDiagnostic, diagnosticKnown, lastPending, pendingKnown)
			return out, fmt.Errorf("%w: %s: %w", ErrVerificationPending, out.Reason, err)
		}
		token, provider, reason := s.inspect(ctx, target)
		pending, known := s.challengePending(ctx)
		diagnostic, currentDocumentKnown := s.currentDocumentDiagnostic(ctx)
		if known {
			lastPending, pendingKnown = pending, true
		}
		if currentDocumentKnown {
			lastDiagnostic, diagnosticKnown = diagnostic, true
		}
		if ctx.Err() != nil {
			continue
		}
		observedChallenge = observedChallenge || pending || (currentDocumentKnown && diagnostic.Challenge)
		if known && !pending && currentDocumentKnown && !diagnostic.Challenge && diagnostic.Status >= 400 {
			out.Reason = verificationDetail("document refused verification", diagnostic, true, false, true)
			return out, fmt.Errorf("%w: %s", ErrVerificationRejected, out.Reason)
		}
		networkFailed := currentDocumentKnown && (diagnostic.NetworkError != "" || diagnostic.BlockedReason != "" || diagnostic.CORSError != "")
		if provider == "cloudflare" && oldClearance[token] {
			// A revoked/insufficient clearance cookie must not immediately end a
			// new challenge; its continued presence is not fresh provider approval.
			token = ""
		}
		if token != "" && !networkFailed {
			out.Solved = true
			out.Provider = provider
			out.Token = token
			out.SessionBound = true
			out.Reason = reason
			return out, nil
		}
		normalDocument := currentDocumentKnown && diagnostic.Status >= 200 && diagnostic.Status < 300 && !diagnostic.Challenge
		if known && !pending && !networkFailed && (observedChallenge || normalDocument) {
			out.RetryReady, out.SessionBound = true, true
			out.Reason = verificationDetail("document ready for protected request verification in the same browser", diagnostic, currentDocumentKnown, false, true)
			return out, nil
		}
		if pending && !prompted && time.Since(started) >= 1500*time.Millisecond {
			prompted = true
			prompt := interactionPrompt(nil)
			if presenter, ok := s.Session.(interface{ Present(context.Context) error }); ok {
				prompt = interactionPrompt(presenter.Present(ctx))
			}
			if s.OnInteractionRequired != nil {
				s.OnInteractionRequired(prompt)
			}
		}
		select {
		case <-ctx.Done():
			out.Reason = verificationDetail("provider verification incomplete", lastDiagnostic, diagnosticKnown, lastPending, pendingKnown)
			return out, fmt.Errorf("%w: %s: %w", ErrVerificationPending, out.Reason, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Present cannot turn an existing headless process into a desktop browser.
// Keep restart/display requirements explicit, without exposing a raw CDP error
// or suggesting that changing an environment variable fixes a running session.
func interactionPrompt(presentErr error) string {
	if errors.Is(presentErr, browser.ErrInteractiveBrowserRequired) {
		return "当前任务的 Chromium 是无头会话，无法在运行中切换为可视窗口。请在具备图形桌面的环境中以 TOSUB2_BROWSER_HEADLESS=0 重启服务，再重试任务以创建新的浏览器会话。Docker 容器还需要可访问的桌面/显示服务，仅修改该环境变量不够；macOS 可使用当前登录桌面中的原生服务。请由账号本人在新窗口完成网站验证。"
	}
	if presentErr != nil {
		return "网站验证尚未完成，当前 Chromium 窗口无法显示。请确认服务运行在可访问的图形桌面中；Docker 容器需要桌面/显示服务，macOS 可使用当前登录桌面中的原生服务。程序仍在等待当前浏览器中的网站结果。"
	}
	return "请由账号本人在当前 Chromium 窗口中完成网站验证，程序将等待网站返回结果。"
}

func (s BrowserSolver) inspect(ctx context.Context, target string) (token, provider, reason string) {

	// Read common token locations in one browser execution. This also crosses
	// same-origin Turnstile iframes via frame traversal; cross-origin frames are
	// intentionally left to the provider callback and cannot be read by policy.
	v, err := s.Session.Evaluate(ctx, `(function(){
      const pick=(x)=>typeof x==='string'&&x.trim()?x.trim():'';
      const q=['input[name="cf-turnstile-response"]','textarea[name="cf-turnstile-response"]','input[name="g-recaptcha-response"]'];
      for(const sel of q){const e=document.querySelector(sel); const x=pick(e&&e.value)||pick(e&&e.textContent); if(x) return {token:x,provider:'turnstile',reason:'challenge response observed'};}
      return {token:'',provider:'chromium',reason:'challenge pending'};
    })()`)
	if err != nil {
		return "", "chromium", err.Error()
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", "chromium", "challenge pending"
	}
	token, _ = m["token"].(string)
	provider, _ = m["provider"].(string)
	reason, _ = m["reason"].(string)
	if token != "" {
		if provider == "" {
			provider = "chromium"
		}
		return token, provider, reason
	}
	// cf_clearance is normally HttpOnly, so document.cookie is insufficient.
	// Restrict the cookie to the target host and ignore expired clearance.
	if u, err := url.Parse(target); err == nil {
		if cookies, err := s.cookies(ctx); err == nil {
			for _, c := range cookies {
				name, _ := c["name"].(string)
				domain, _ := c["domain"].(string)
				value, _ := c["value"].(string)
				expires, _ := c["expires"].(float64)
				secure, _ := c["secure"].(bool)
				path, _ := c["path"].(string)
				host := strings.ToLower(u.Hostname())
				bare := strings.ToLower(strings.TrimPrefix(domain, "."))
				matches := host == bare || (strings.HasPrefix(domain, ".") && strings.HasSuffix(host, "."+bare))
				pathMatches := path == "" || path == "/" || u.Path == path || strings.HasPrefix(u.Path, strings.TrimSuffix(path, "/")+"/")
				if name == "cf_clearance" && value != "" && matches && pathMatches && (!secure || u.Scheme == "https") && (expires <= 0 || expires > float64(time.Now().Unix())) {
					return value, "cloudflare", "clearance observed; protected request must be verified in the same browser"
				}
			}
		}
	}

	// Cross-origin Turnstile/Sentinel frames cannot be traversed from the page
	// context. CDP can create an isolated world for each frame, so inspect those
	// frames in the same browser session as a fallback.
	if frames, ok := s.Session.(interface {
		FrameURLs(context.Context) ([]string, error)
	}); ok {
		if urls, err := frames.FrameURLs(ctx); err == nil {
			for _, u := range urls {
				if strings.Contains(strings.ToLower(u), "turnstile") || strings.Contains(strings.ToLower(u), "sentinel") || strings.Contains(strings.ToLower(u), "challenge") {
					if v, err := s.Session.EvaluateFrame(ctx, u, `(()=>{const q=['input[name="cf-turnstile-response"]','textarea[name="cf-turnstile-response"]','input[name="g-recaptcha-response"]'];for(const sel of q){const e=document.querySelector(sel);if(e&&e.value)return e.value}return ''})()`); err == nil {
						if x, ok := v.(string); ok && strings.TrimSpace(x) != "" {
							return x, "turnstile", "challenge token observed in iframe"
						}
					}
				}
			}
		}
	}
	if provider == "" {
		provider = "chromium"
	}
	return token, provider, reason
}

func (s BrowserSolver) cookies(ctx context.Context) ([]map[string]any, error) {
	if contextual, ok := s.Session.(interface {
		CookiesContext(context.Context) ([]map[string]any, error)
	}); ok {
		return contextual.CookiesContext(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.Session.Cookies()
}

func (s BrowserSolver) challengePending(ctx context.Context) (pending, known bool) {
	value, err := s.Session.Evaluate(ctx, `(()=>{const title=document.title||'';return !!(window._cf_chl_opt||/just a moment|checking your browser|verify you are human/i.test(title)||document.querySelector('#challenge-running,#challenge-stage,form#challenge-form,iframe[src*="challenges.cloudflare.com"],iframe[src*="turnstile"],input[name="cf-turnstile-response"],textarea[name="cf-turnstile-response"]'))})()`)
	if err != nil {
		return false, false
	}
	pending, known = value.(bool)
	return pending, known
}
