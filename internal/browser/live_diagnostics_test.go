package browser

import (
	"context"
	"os"
	"testing"
	"time"
)

// Explicit opt-in public navigation only: no account, form, challenge response
// or SMS submission. No document, inline scripts, query strings or cookies log.
func TestLivePublicLoginDocumentDiagnostics(t *testing.T) {
	if os.Getenv("TOSUB2_LIVE_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_LIVE_BROWSER_TEST=1 to inspect the public upstream login document")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := NewManager(path, 1)
	defer manager.Close()
	s, err := manager.Start(ctx, "", "", "chromium-native", "")
	if err != nil {
		t.Fatal(err)
	}
	const target = "https://auth.openai.com/log-in"
	if err = s.Navigate(ctx, target); err != nil {
		t.Fatal(err)
	}
	t.Logf("public login document: %s", s.ResourceDiagnostic(target).Summary())
	value, err := s.Evaluate(ctx, `(()=>{const b=document.body?document.body.innerText:'';const scripts=Array.from(document.scripts);return {origin:location.origin,loginPath:location.pathname==='/log-in',hasSDK:!!window.SentinelSDK,scriptOrigins:Array.from(new Set(scripts.map(s=>{try{return new URL(s.src).origin}catch{return ''}}))).filter(Boolean),sentinelPaths:scripts.map(s=>{try{const u=new URL(s.src);return u.hostname==='sentinel.openai.com'&&u.pathname.startsWith('/backend-api/sentinel/')&&u.pathname.split('/').every(p=>!p||/^[a-zA-Z0-9_.-]+$/.test(p))?u.pathname:''}catch{return ''}}).filter(Boolean),inlineSentinelReference:scripts.some(s=>!s.src&&s.textContent.includes('Sentinel')),hasLoginInput:!!document.querySelector('input[type=email],input[name=email]'),challengeMarker:!!(window._cf_chl_opt||document.querySelector('#challenge-running,#challenge-stage,form#challenge-form')||Array.from(document.querySelectorAll('iframe')).some(f=>f.src.includes('challenges.cloudflare.com'))),unsupportedCountry:/unsupported.country|country.*not.*supported|region.*not.*supported/i.test(b),accessDenied:/access denied|do not have access|request blocked/i.test(b),cspMeta:!!document.querySelector('meta[http-equiv=Content-Security-Policy]')}})()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("safe public document classifications: %#v", value)
}
