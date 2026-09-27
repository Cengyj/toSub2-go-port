package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/transport"
)

func TestChromiumCDPIntegration(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 to run Chromium integration test")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	m := NewManager(path, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s, err := m.Start(ctx, "", "", "", "")
	if err != nil {
		t.Fatalf("Chromium launch or CDP failed: %v", err)
	}
	defer m.Close()
	srv := httptest.NewServer(httpHandler())
	defer srv.Close()
	if err := s.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	v, err := s.Evaluate(ctx, "document.querySelector('#status').textContent")
	if err != nil || v != "ready" {
		t.Fatalf("evaluate: value=%v err=%v", v, err)
	}
	html, err := s.HTML(ctx)
	if err != nil || !strings.Contains(html, "id=\"status\"") {
		t.Fatalf("html: err=%v", err)
	}
	if err := s.SetCookies([]map[string]any{{"name": "cdp_test", "value": "ok", "url": srv.URL, "path": "/"}}); err != nil {
		t.Fatal(err)
	}
	cookies, err := s.Cookies()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cookies {
		if c["name"] == "cdp_test" && c["value"] == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("set cookie not returned: %#v", cookies)
	}
}

// Small local page exercises DOM and navigation without external credentials.
func httpHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body><div id="status">ready</div></body></html>`))
	})
}

func integrationSession(t *testing.T) (*Session, context.Context) {
	t.Helper()
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 to run real Chromium integration")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("no installed Chromium; set CHROME_PATH")
	}
	m := NewManager(path, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	s, e := m.Start(ctx, "", "", "", "fixture-device")
	if e != nil {
		cancel()
		m.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close(); cancel() })
	return s, ctx
}

func TestBrowserTransportManualRedirectCookiesAndMethod(t *testing.T) {
	s, ctx := integrationSession(t)
	var redirected atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			w.Header().Add("Set-Cookie", "first=one; Path=/; HttpOnly")
			w.Header().Add("Set-Cookie", "second=two; Path=/")
			w.Header().Set("Location", "/unwanted")
			w.WriteHeader(302)
		case "/unwanted":
			redirected.Add(1)
			w.WriteHeader(500)
		case "/delete":
			w.Header().Set("Set-Cookie", "first=; Max-Age=0; Path=/")
			w.WriteHeader(204)
		case "/echo":
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"method": r.Method, "body": string(b), "cookie": r.Header.Get("Cookie"), "ua": r.UserAgent(), "custom": r.Header.Get("X-Test")})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	jar := transport.NewCookieJar(transport.Cookie{Name: "imported", Value: "existing", Domain: "127.0.0.1", Path: "/", HostOnly: true})
	bt := NewTransport(s, jar)
	r, e := bt.Do(ctx, transport.Request{URL: srv.URL + "/redirect"})
	if e != nil {
		t.Fatal(e)
	}
	if r.StatusCode != 302 || r.Header.Get("Location") != "/unwanted" || len(r.RawSetCookie) != 2 {
		t.Fatalf("redirect metadata: %+v", r)
	}
	if redirected.Load() != 0 {
		t.Fatal("Chromium followed redirect")
	}
	if !jar.Has("first") || !jar.Has("second") {
		t.Fatal("response cookies not copied into jar")
	}
	r, e = bt.Do(ctx, transport.Request{Method: "POST", URL: srv.URL + "/echo", Headers: http.Header{"Content-Type": {"application/json"}, "X-Test": {"custom"}}, Body: []byte(`{"foo":"bar"}`)})
	if e != nil {
		t.Fatal(e)
	}
	var got map[string]any
	if e = json.Unmarshal(r.Body, &got); e != nil {
		t.Fatal(e)
	}
	if got["method"] != "POST" || got["body"] != `{"foo":"bar"}` || got["custom"] != "custom" || got["ua"] != s.Identity().UserAgent {
		t.Fatalf("request data incorrect: %+v", got)
	}
	if !strings.Contains(got["cookie"].(string), "first=one") || !strings.Contains(got["cookie"].(string), "imported=existing") {
		t.Fatal("browser cookie not sent")
	}
	if _, e = bt.Do(ctx, transport.Request{URL: srv.URL + "/delete"}); e != nil {
		t.Fatal(e)
	}
	if jar.Has("first") {
		t.Fatal("deleted browser cookie remained in jar")
	}
}

func TestBrowserSessionLifecycleAndCancellation(t *testing.T) {
	s, ctx := integrationSession(t)
	dir := s.Dir
	m := s.owner
	if _, e := m.Start(ctx, "", "", "", ""); e == nil {
		t.Fatal("concurrency limit not enforced")
	}
	if _, e := s.Evaluate(ctx, "throw new Error('secret must not be returned')"); e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe JS error")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := s.Evaluate(cancelCtx, "1"); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation: %v", e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("browser profile not removed")
	}
	if _, e := s.Evaluate(ctx, "1"); !errors.Is(e, ErrClosed) {
		t.Fatalf("closed session: %v", e)
	}
	replacement, e := m.Start(ctx, "", "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	_ = replacement.Close()
}

func TestProxyParsingAndCredentials(t *testing.T) {
	endpoint, user, pass, e := parseProxy("http://user:password@127.0.0.1:8080")
	if e != nil || endpoint != "http://127.0.0.1:8080" || user != "user" || pass != "password" {
		t.Fatal("proxy parsing failed")
	}
	if _, user, pass, e := parseProxy("socks5h://user:password@localhost:123"); e != nil || user != "user" || pass != "password" {
		t.Fatal("SOCKS credentials not preserved for bridge")
	}
	if endpoint, _, _, e = parseProxy("socks5h://localhost:123"); e != nil || endpoint != "socks5://localhost:123" {
		t.Fatal("SOCKS scheme not normalized")
	}
}

func TestBrowserAuthenticatedHTTPProxy(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for Chromium proxy integration")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed")
	}
	var challenges atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("fixture-user:fixture-pass")) {
			challenges.Add(1)
			w.Header().Set("Proxy-Authenticate", `Basic realm="fixture-proxy"`)
			w.WriteHeader(407)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("proxy-authorized"))
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("fixture-user", "fixture-pass")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := NewManager(path, 1)
	defer m.Close()
	s, e := m.Start(ctx, u.String(), "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	r, e := NewTransport(s, nil).Do(ctx, transport.Request{URL: "http://fixture.example/proxy-check"})
	if e != nil {
		t.Fatal(e)
	}
	if string(r.Body) != "proxy-authorized" || challenges.Load() == 0 {
		t.Fatal("proxy authentication was not exercised")
	}
}

func TestChromiumLateDeviceBinding(t *testing.T) {
	s, ctx := integrationSession(t)
	// The helper session starts with a device. A different identifier cannot be
	// silently assigned after a browser challenge has already bound identity.
	if e := s.UpdateDeviceID(ctx, "different-device"); e == nil {
		t.Fatal("different device accepted")
	}
	_ = s.Close()
	m := s.owner
	late, e := m.Start(ctx, "", "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	defer late.Close()
	srv := httptest.NewServer(httpHandler())
	defer srv.Close()
	if e = late.Navigate(ctx, srv.URL); e != nil {
		t.Fatal(e)
	}
	if e = late.UpdateDeviceID(ctx, "late-device"); e != nil {
		t.Fatal(e)
	}
	v, e := late.Evaluate(ctx, "localStorage.getItem('oai-did')")
	if e != nil || v != "late-device" {
		t.Fatal("current page device not bound")
	}
	if e = late.Navigate(ctx, srv.URL+"/next"); e != nil {
		t.Fatal(e)
	}
	v, e = late.Evaluate(ctx, "localStorage.getItem('oai-did')")
	if e != nil || v != "late-device" {
		t.Fatal("new document device not bound")
	}
}

func TestChromiumImportsLegacyHostPrefixedSecureCookie(t *testing.T) {
	session, ctx := integrationSession(t)
	// CDP can install an HTTPS cookie without making an external network request.
	// Using a URL (not a Domain attribute) is required for the __Host- prefix.
	var legacy transport.Cookie
	if e := json.Unmarshal([]byte(`{"name":"__Host-next-auth.csrf-token","value":"synthetic-csrf","domain":"fixture.example","path":"/","secure":true,"expires":null}`), &legacy); e != nil {
		t.Fatal(e)
	}
	legacy.HTTPOnly = true
	legacy.SameSite = "Strict"
	jar := transport.NewCookieJar(legacy)
	client := NewTransport(session, jar)
	if e := client.ImportCookies(ctx); e != nil {
		t.Fatal(e)
	}
	raw, e := session.call(ctx, "Network.getCookies", map[string]any{"urls": []string{"https://fixture.example/"}})
	if e != nil {
		t.Fatal(e)
	}
	var result struct {
		Cookies []map[string]any `json:"cookies"`
	}
	if e = json.Unmarshal(raw, &result); e != nil {
		t.Fatal(e)
	}
	found := false
	for _, cookie := range result.Cookies {
		if cookie["name"] == legacy.Name {
			found = true
			if cookie["domain"] != "fixture.example" || cookie["secure"] != true || cookie["httpOnly"] != true || cookie["sameSite"] != "Strict" {
				t.Fatal("Chrome cookie attributes changed")
			}
		}
	}
	if !found {
		t.Fatal("Chrome rejected restored __Host- cookie")
	}
	if e = client.ExportCookies(ctx); e != nil {
		t.Fatal(e)
	}
	cookies := jar.Cookies()
	if len(cookies) != 1 || !cookies[0].HostOnly || !cookies[0].HTTPOnly || cookies[0].SameSite != "Strict" {
		t.Fatal("browser export dropped cookie security attributes")
	}
}
