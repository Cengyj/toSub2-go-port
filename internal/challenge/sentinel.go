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

// Sentinel runs the provider's current loader and SDK in the page's own real
// browser context. SDK, iframe, observer and fetch state remain in that page.
// A failed or abandoned invocation aborts page requests and unloads its document;
// a timed-out SDK Promise is never reused for another flow.
func (s BrowserSolver) Sentinel(parent context.Context, pageURL, loaderURL, flow string) (Result, error) {
	out := Result{Provider: "sentinel"}
	if s.Session == nil {
		return out, errors.New("BROWSER_REQUIRED: Chromium session is required")
	}
	if loaderURL == "" {
		loaderURL = "https://sentinel.openai.com/backend-api/sentinel/sdk.js"
	}
	for _, raw := range []string{pageURL, loaderURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return out, errors.New("invalid Sentinel page or loader URL")
		}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	current, err := s.Session.Evaluate(ctx, "location.href")
	if err != nil {
		return out, err
	}
	if current != pageURL {
		if err = s.Session.Navigate(ctx, pageURL); err != nil {
			return out, err
		}
	}
	// A challenge document is not the login document. Its restrictive CSP
	// correctly rejects injected application scripts; loading an SDK there
	// would misreport the provider gate as a broken Sentinel loader. Keep the
	// same Chromium page for the site's normal verification or user interaction.
	if err = s.ensureSentinelDocument(ctx, pageURL); err != nil {
		return out, err
	}
	maxAge := s.LoaderMaxAge
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}
	// The marker is page-local and contains no token or credential. It also
	// prevents reusing a previous pending call after Go context cancellation.
	state, err := s.Session.Evaluate(ctx, `(()=>{const s=window.__tosub2SentinelRuntime;return s?{loader:s.loader,loadedAt:s.loadedAt,failed:!!s.failed,inFlight:!!s.inFlight}:null})()`)
	if err != nil {
		return out, err
	}
	forceLoader := false
	if m, ok := state.(map[string]any); ok {
		old, _ := m["loader"].(string)
		loaded, _ := m["loadedAt"].(float64)
		failed, _ := m["failed"].(bool)
		pending, _ := m["inFlight"].(bool)
		forceLoader = old != loaderURL
		stale := loaded > 0 && time.Since(time.UnixMilli(int64(loaded))) > maxAge
		if forceLoader || failed || pending || stale {
			if reloader, ok := s.Session.(interface {
				Reload(context.Context, bool) error
			}); ok {
				err = reloader.Reload(ctx, true)
			} else {
				err = s.Session.Navigate(ctx, "about:blank")
				if err == nil {
					err = s.Session.Navigate(ctx, pageURL)
				}
			}
			if err != nil {
				return out, err
			}
			if err = s.ensureSentinelDocument(ctx, pageURL); err != nil {
				return out, err
			}
		}
	}
	deadline, _ := ctx.Deadline()
	budget := time.Until(deadline).Milliseconds()
	if budget <= 0 {
		return out, ctx.Err()
	}
	expression := `(async()=>{
 const loader=` + jsQuote(loaderURL) + `,flow=` + jsQuote(flow) + `,budget=` + jsonNumber(budget) + `,force=` + boolJS(forceLoader) + `;
 const until=Date.now()+budget;
 const state=window.__tosub2SentinelRuntime||(window.__tosub2SentinelRuntime={loader,loadedAt:0,failed:false,inFlight:false});
 state.loader=loader;state.inFlight=true;
 const bounded=(work,label,cap=Infinity)=>{let timer;const remaining=Math.max(1,Math.min(cap,until-Date.now()));return Promise.race([Promise.resolve(work),new Promise((_,reject)=>{timer=setTimeout(()=>reject(new Error(label)),remaining)})]).finally(()=>clearTimeout(timer))};
 const waitSDK=old=>new Promise((resolve,reject)=>{const tick=()=>{const sdk=window.SentinelSDK;if(sdk&&typeof sdk.token==='function'&&(!old||sdk!==old))return resolve(sdk);if(Date.now()>=until)return reject(new Error('sdk_timeout'));setTimeout(tick,25)};tick()});
 try{
  const previous=window.SentinelSDK;
  if(force||!previous||typeof previous.token!=='function'){
   let script;try{await bounded(new Promise((resolve,reject)=>{script=document.createElement('script');script.src=loader;script.onload=resolve;script.onerror=()=>reject(new Error('loader_failed'));(document.head||document.documentElement).appendChild(script)}),'loader_timeout')}finally{if(script){script.onload=null;script.onerror=null}}
  }
  const sdk=await waitSDK(force?previous:null);if(!state.loadedAt)state.loadedAt=Date.now();
  const token=await bounded(Promise.resolve().then(()=>sdk.token(flow)),'token_timeout');
  let so='',observer='unsupported';
  if(typeof sdk.sessionObserverToken==='function'){try{const value=await bounded(Promise.resolve().then(()=>sdk.sessionObserverToken(flow)),'observer_timeout',5000);so=typeof value==='string'?value:'';observer=so?'available':'empty'}catch(_){observer='failed'}}
  state.failed=false;return {token:typeof token==='string'?token:'',so,observer};
 }catch(error){state.failed=true;const allowed=['loader_failed','loader_timeout','sdk_timeout','token_timeout'];return {error:allowed.includes(error&&error.message)?error.message:'sdk_failed'}}
 finally{state.inFlight=false}
})()`
	value, err := s.Session.Evaluate(ctx, expression)
	if err != nil {
		s.discardSentinelRuntime()
		return out, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return out, errors.New("SENTINEL_INVALID_RESULT: SDK did not return an object")
	}
	if code, _ := result["error"].(string); code != "" {
		detail := ""
		if resources, ok := s.Session.(interface {
			ResourceDiagnostic(string) browser.NetworkDiagnostic
		}); ok {
			if diagnostic := resources.ResourceDiagnostic(loaderURL).Summary(); diagnostic != "" {
				detail = " (loader: " + diagnostic + ")"
			}
		}
		s.discardSentinelRuntime()
		return out, errors.New("SENTINEL_SDK_FAILED: " + safeSDKError(code) + detail)
	}
	out.Token, _ = result["token"].(string)
	out.SessionObserverToken, _ = result["so"].(string)
	out.Token = strings.TrimSpace(out.Token)
	out.SessionObserverToken = strings.TrimSpace(out.SessionObserverToken)
	out.SessionObserverStatus, _ = result["observer"].(string)
	if err = validateSentinelToken(out.Token, flow, false); err != nil {
		return Result{Provider: "sentinel"}, err
	}
	if out.SessionObserverToken != "" {
		if err = validateSentinelToken(out.SessionObserverToken, flow, true); err != nil {
			out.SessionObserverToken = ""
			out.SessionObserverStatus = "invalid"
		}
	}
	if s.RequireSessionObserver && out.SessionObserverStatus != "available" {
		s.discardSentinelRuntime()
		return Result{Provider: "sentinel", SessionObserverStatus: out.SessionObserverStatus}, errors.New("SENTINEL_OBSERVER_REQUIRED: SDK session observer token unavailable")
	}
	out.Solved = true
	out.SessionBound = true
	out.Reason = "Sentinel SDK token generated in Chromium"
	return out, nil
}

func (s BrowserSolver) ensureSentinelDocument(ctx context.Context, pageURL string) error {
	blocked, detail, err := s.sentinelPageBlocked(ctx)
	if err != nil || !blocked {
		return err
	}
	result, err := s.waitForChallenge(ctx, pageURL, s.clearanceSnapshot(ctx))
	if err != nil {
		return fmt.Errorf("SENTINEL_PAGE_CHALLENGE: login document verification incomplete%s: %w", detail, err)
	}
	if !result.Solved && !result.RetryReady {
		return fmt.Errorf("%w: SENTINEL_PAGE_CHALLENGE: document is not ready for verification%s", ErrVerificationPending, detail)
	}
	// An observed cookie/token or disappeared DOM only permits verification.
	// The provider must serve the login document in this same browser first.
	if err = s.Session.Navigate(ctx, pageURL); err != nil {
		if cause := verificationContextCause(ctx, err); cause != nil {
			return fmt.Errorf("%w: SENTINEL_PAGE_CHALLENGE: document verification navigation incomplete: %w", ErrVerificationPending, cause)
		}
		return errors.New("SENTINEL_PAGE_CHALLENGE: document verification navigation failed")
	}
	stillBlocked, fresh, err := s.sentinelPageBlocked(ctx)
	if err != nil {
		return err
	}
	if stillBlocked {
		return fmt.Errorf("%w: SENTINEL_PAGE_CHALLENGE: provider still requires login document verification%s", ErrVerificationPending, fresh)
	}
	diagnostic, documentKnown := s.currentDocumentDiagnostic(ctx)
	pending, domKnown := s.challengePending(ctx)
	if cause := ctx.Err(); cause != nil {
		return fmt.Errorf("%w: SENTINEL_PAGE_CHALLENGE: %s: %w", ErrVerificationPending, verificationDetail("normal login document not verified", diagnostic, documentKnown, pending, domKnown), cause)
	}
	if !documentKnown || diagnostic.Status < 200 || diagnostic.Status >= 300 || diagnostic.Challenge || diagnostic.NetworkError != "" || diagnostic.BlockedReason != "" || diagnostic.CORSError != "" || !domKnown || pending {
		return fmt.Errorf("%w: SENTINEL_PAGE_CHALLENGE: %s", ErrVerificationPending, verificationDetail("normal login document not verified", diagnostic, documentKnown, pending, domKnown))
	}
	return nil
}

func (s BrowserSolver) sentinelPageBlocked(ctx context.Context) (bool, string, error) {
	detail := ""
	diagnostic, documentKnown := s.currentDocumentDiagnostic(ctx)
	if documentKnown {
		if summary := diagnostic.Summary(); summary != "" {
			detail = " (document: " + summary + ")"
		}
	}
	v, err := s.Session.Evaluate(ctx, `(()=>{const title=document.title||'';return !!(window._cf_chl_opt||document.querySelector('#challenge-running,#challenge-stage,form#challenge-form')||(/just a moment|checking your browser|verify you are human/i.test(title)&&!document.querySelector('input[type=email],input[name=email]')))})()`)
	if cause := verificationContextCause(ctx, err); cause != nil {
		return false, detail, fmt.Errorf("%w: SENTINEL_PAGE_CHALLENGE: document observation incomplete%s: %w", ErrVerificationPending, detail, cause)
	}
	pending, domKnown := v.(bool)
	domKnown = domKnown && err == nil
	if documentKnown && domKnown && !pending && !diagnostic.Challenge && diagnostic.Status >= 400 {
		return false, detail, fmt.Errorf("%w: SENTINEL_PAGE_REJECTED: %s", ErrVerificationRejected, verificationDetail("login document refused verification", diagnostic, true, false, true))
	}
	return (documentKnown && diagnostic.Challenge) || (domKnown && pending), detail, nil
}

// Runtime.evaluate cancellation stops Go waiting, not JavaScript execution.
// Unload the failed document so SDK fetch/XHR/timers cannot complete an old
// flow after its caller has abandoned it. Cookies, proxy and browser identity
// remain in this context for an explicit subsequent authorized attempt.
func (s BrowserSolver) discardSentinelRuntime() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if discard, ok := s.Session.(interface{ DiscardRuntime(context.Context) error }); ok {
		err = discard.DiscardRuntime(ctx)
	} else {
		err = s.Session.Navigate(ctx, "about:blank")
	}
	if err != nil {
		_ = s.Session.Close()
	}
}

func safeSDKError(code string) string {
	switch code {
	case "loader_failed", "loader_timeout", "sdk_timeout", "token_timeout":
		return code
	default:
		return "sdk_failed"
	}
}
func validateSentinelToken(token, flow string, observer bool) error {
	if strings.ContainsAny(token, "\r\n\x00") {
		return errors.New("SENTINEL_TOKEN_INVALID: SDK token contains invalid HTTP header characters")
	}
	var payload map[string]json.RawMessage
	if token == "" || json.Unmarshal([]byte(token), &payload) != nil {
		return errors.New("SENTINEL_TOKEN_INVALID: SDK token is not the expected JSON payload")
	}
	required := []string{"p", "t", "c", "id", "flow"}
	if observer {
		required = []string{"so", "c", "id", "flow"}
	}
	for _, key := range required {
		if _, ok := payload[key]; !ok {
			return errors.New("SENTINEL_TOKEN_INVALID: SDK payload is incomplete")
		}
	}
	var gotFlow string
	if json.Unmarshal(payload["flow"], &gotFlow) != nil || gotFlow != flow {
		return errors.New("SENTINEL_FLOW_MISMATCH: SDK returned a token for a different flow")
	}
	return nil
}
func jsonNumber(value int64) string { b, _ := json.Marshal(value); return string(b) }
func boolJS(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
