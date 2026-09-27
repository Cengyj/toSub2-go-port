package browser

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"tosub2-go-port/internal/transport"
)

func TestBrowserTransportProtocolCacheIsolationAndRedirectVariants(t *testing.T) {
	s, ctx := integrationSession(t)
	var cached, followed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cache":
			cached.Add(1)
			w.Header().Set("Cache-Control", "public, max-age=600")
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("X-Fixture", "cached-response")
			fmt.Fprint(w, "cached-payload")
		case "/redirect":
			status, _ := strconv.Atoi(r.URL.Query().Get("status"))
			w.Header().Set("Location", "/must-not-follow")
			w.Header().Add("Set-Cookie", fmt.Sprintf("redirect%d=accepted; Path=/; HttpOnly", status))
			w.Header().Add("Set-Cookie", "dated=fixture; Path=/; Expires=Wed, 01 Jan 2031 00:00:00 GMT")
			w.WriteHeader(status)
		case "/must-not-follow":
			followed.Add(1)
			w.WriteHeader(500)
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	jar := transport.NewCookieJar()
	bt := NewTransport(s, jar)
	for i := 0; i < 2; i++ {
		r, err := bt.Do(ctx, transport.Request{URL: server.URL + "/cache"})
		if err != nil || string(r.Body) != "cached-payload" || r.Header.Get("X-Fixture") != "cached-response" {
			t.Fatalf("cached request %d failed: %v", i, err)
		}
	}
	for _, status := range []int{301, 302, 303, 307, 308} {
		r, err := bt.Do(ctx, transport.Request{Method: http.MethodPost, URL: fmt.Sprintf("%s/redirect?status=%d", server.URL, status), Body: []byte("fixture-body")})
		if err != nil {
			t.Fatalf("redirect %d: %v", status, err)
		}
		if r.StatusCode != status || r.Header.Get("Location") != "/must-not-follow" || len(r.RawSetCookie) != 2 || !jar.Has(fmt.Sprintf("redirect%d", status)) {
			t.Fatalf("redirect %d lost manual metadata or cookies", status)
		}
	}
	if followed.Load() != 0 {
		t.Fatal("Chromium followed a manual redirect")
	}
	if cached.Load() != 2 {
		t.Fatal("protocol transport reused a cached HTTP response")
	}
}

func TestBrowserProtocolTargetDoesNotChangeSDKServiceWorker(t *testing.T) {
	s, ctx := integrationSession(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/setup":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><body>authorized worker fixture</body>")
		case "/sw.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, `self.addEventListener('install',e=>e.waitUntil(self.skipWaiting()));self.addEventListener('activate',e=>e.waitUntil(self.clients.claim()));self.addEventListener('fetch',e=>{if(new URL(e.request.url).pathname==='/api')e.respondWith(Promise.resolve(new Response('worker-response',{headers:{'Content-Type':'text/plain'}})))})`)
		case "/api":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "network-response")
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	if err := s.Navigate(ctx, server.URL+"/setup"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evaluate(ctx, `(async()=>{await navigator.serviceWorker.register('/sw.js');await navigator.serviceWorker.ready;if(!navigator.serviceWorker.controller)await new Promise(resolve=>navigator.serviceWorker.addEventListener('controllerchange',resolve,{once:true}));return true})()`); err != nil {
		t.Fatal(err)
	}
	if value, err := s.Evaluate(ctx, `fetch('/api').then(r=>r.text())`); err != nil || value != "worker-response" {
		t.Fatalf("SDK page worker: %v value=%v", err, value)
	}
	response, err := NewTransport(s, nil).Do(ctx, transport.Request{URL: server.URL + "/api"})
	if err != nil || string(response.Body) != "network-response" {
		t.Fatalf("direct protocol request was rewritten by worker: %v", err)
	}
	if value, err := s.Evaluate(ctx, `fetch('/api').then(r=>r.text())`); err != nil || value != "worker-response" {
		t.Fatal("protocol request changed the live SDK service worker behavior")
	}
}
