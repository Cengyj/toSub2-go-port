package challenge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
	"tosub2-go-port/internal/transport"
)

// This fixture validates the actual Chromium loader->versioned SDK->cross-origin
// iframe/postMessage path. It does not claim to validate the live OpenAI SDK.
func TestChromiumDynamicSDKAndSessionObserver(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium dynamic SDK fixture")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = browser.DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	var iframeLoads atomic.Int32
	frame := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iframeLoads.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><div id="frame-ready">ready</div><script>addEventListener('message',async e=>{if(e.data.kind!=='fixture')return;const body=await fetch('/payload').then(r=>r.text());e.source.postMessage({kind:'fixture-result',nonce:e.data.nonce,body},e.origin)});</script>`)
	}))
	defer frame.Close()
	frameURL := strings.Replace(frame.URL, "127.0.0.1", "localhost", 1) + "/frame"
	var loaderLoads, sdkLoads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><html><head></head><body>authorized test page</body></html>")
	})
	mux.HandleFunc("/loader.js", func(w http.ResponseWriter, r *http.Request) {
		loaderLoads.Add(1)
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, `(()=>{const s=document.createElement('script');s.src='/sentinel/version-fixture/sdk.js';document.head.appendChild(s)})();`)
	})
	mux.HandleFunc("/sentinel/version-fixture/sdk.js", func(w http.ResponseWriter, r *http.Request) {
		sdkLoads.Add(1)
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprintf(w, `(()=>{
 const frame=document.createElement('iframe');frame.src=%q;document.body.appendChild(frame);
 const ready=new Promise(resolve=>frame.addEventListener('load',resolve));
 const request=async(flow)=>{await ready;const nonce=crypto.getRandomValues(new Uint32Array(1))[0];return await new Promise(resolve=>{const receive=e=>{if(e.source===frame.contentWindow&&e.data.kind==='fixture-result'&&e.data.nonce===nonce){removeEventListener('message',receive);resolve(JSON.stringify({p:'fixture',t:'fixture',c:'fixture',id:localStorage.getItem('oai-did'),flow}))}};addEventListener('message',receive);frame.contentWindow.postMessage({kind:'fixture',nonce},'*')})};
 const observer=new MutationObserver(()=>{});observer.observe(document.body,{childList:true});
 window.SentinelSDK={token:request,sessionObserverToken:async flow=>JSON.stringify({so:'fixture-so',c:'fixture',id:localStorage.getItem('oai-did'),flow})};
 })()`, frameURL)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := browser.NewManager(path, 1)
	defer m.Close()
	s, e := m.Start(ctx, "", "", "", "fixture-device")
	if e != nil {
		t.Fatal(e)
	}
	result, e := (BrowserSolver{Session: s}).Sentinel(ctx, srv.URL+"/page", srv.URL+"/loader.js", "email_otp_validate")
	if e != nil {
		t.Fatal(e)
	}
	if !result.SessionBound || !strings.Contains(result.Token, "fixture-device") || !strings.Contains(result.SessionObserverToken, "fixture-so") {
		t.Fatal("SDK token/observer result missing")
	}
	if loaderLoads.Load() != 1 || sdkLoads.Load() != 1 || iframeLoads.Load() < 1 {
		t.Fatal("dynamic loader/SDK/iframe were not loaded")
	}
	result, e = (BrowserSolver{Session: s, RequireSessionObserver: true}).Sentinel(ctx, srv.URL+"/page", srv.URL+"/loader.js", "password_verify")
	if e != nil || result.SessionObserverStatus != "available" || !strings.Contains(result.Token, "password_verify") {
		t.Fatalf("second SDK flow: %v", e)
	}
	if loaderLoads.Load() != 1 || sdkLoads.Load() != 1 {
		t.Fatal("repeated flow replaced the live SDK session")
	}
	v, e := s.EvaluateFrame(ctx, frameURL, "document.querySelector('#frame-ready').textContent")
	if e != nil || v != "ready" {
		t.Fatalf("frame DOM access: %v", e)
	}
	// The protocol request stays in Chromium and does not destroy the live SDK.
	bt := browser.NewTransport(s, transport.NewCookieJar())
	_, e = bt.Do(ctx, transport.Request{URL: srv.URL + "/page"})
	if e != nil {
		t.Fatal(e)
	}
	v, e = s.Evaluate(ctx, "typeof window.SentinelSDK.token")
	if e != nil || v != "function" {
		t.Fatal("browser request destroyed SDK page")
	}
}

func TestChromiumCloudflareHttpOnlyAndTurnstileCallbackFixtures(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for challenge browser fixture")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = browser.DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/cloudflare" {
			http.SetCookie(w, &http.Cookie{Name: "cf_clearance", Value: "local-fixture-clearance", Path: "/", HttpOnly: true})
			fmt.Fprint(w, "<!doctype html><title>Fixture verified</title>")
			return
		}
		fmt.Fprint(w, `<!doctype html><input type="hidden" name="cf-turnstile-response"><script>setTimeout(()=>{document.querySelector('input').value='local-fixture-turnstile'},40)</script>`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := browser.NewManager(path, 1)
	defer m.Close()
	s, e := m.Start(ctx, "", "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	result, e := (BrowserSolver{Session: s}).SolveContext(ctx, server.URL+"/cloudflare")
	if e != nil || result.Provider != "cloudflare" || result.Token != "local-fixture-clearance" {
		t.Fatalf("HttpOnly clearance: %v %s", e, result.Provider)
	}
	// A different host must not inherit the clearance result from 127.0.0.1.
	otherHost := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	result, e = (BrowserSolver{Session: s}).SolveContext(ctx, otherHost+"/turnstile")
	if e != nil || result.Provider != "turnstile" || result.Token != "local-fixture-turnstile" {
		t.Fatalf("turnstile callback: %v %s", e, result.Provider)
	}
}
