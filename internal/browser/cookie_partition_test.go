package browser

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"tosub2-go-port/internal/transport"
)

func TestChromiumPartitionedCookieCheckpointRoundtrip(t *testing.T) {
	s, ctx := integrationSession(t)
	ordinary := transport.Cookie{Name: "sid", Value: "ordinary", Domain: "auth.example", Path: "/", HostOnly: true, Secure: true, HTTPOnly: true, SameSite: "None"}
	first := ordinary
	first.Value = "first"
	first.PartitionKey = &transport.CookiePartitionKey{TopLevelSite: "https://first.example", HasCrossSiteAncestor: true}
	second := first
	second.Value = "second"
	second.PartitionKey = &transport.CookiePartitionKey{TopLevelSite: "https://second.example", HasCrossSiteAncestor: true}
	jar := transport.NewCookieJar(ordinary, first, second)
	bt := NewTransport(s, jar)
	if err := bt.ImportCookies(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bt.ExportCookies(ctx); err != nil {
		t.Fatal(err)
	}
	if len(jar.Cookies()) != 3 {
		t.Fatalf("Chrome roundtrip lost separate cookie identities: %d", len(jar.Cookies()))
	}
	var keys []string
	for _, c := range jar.Cookies() {
		if c.PartitionKey != nil {
			keys = append(keys, c.PartitionKey.TopLevelSite)
		}
	}
	if len(keys) != 2 {
		t.Fatal("Chrome partition keys were not preserved")
	}
	wire, err := json.Marshal(jar.Cookies())
	if err != nil {
		t.Fatal(err)
	}
	var saved []transport.Cookie
	if err = json.Unmarshal(wire, &saved); err != nil {
		t.Fatal(err)
	}
	if _, err = s.call(ctx, "Network.clearBrowserCookies", nil); err != nil {
		t.Fatal(err)
	}
	restored := transport.NewCookieJar(saved...)
	next := NewTransport(s, restored)
	if err = next.ImportCookies(ctx); err != nil {
		t.Fatal(err)
	}
	if err = next.ExportCookies(ctx); err != nil {
		t.Fatal(err)
	}
	if len(restored.Cookies()) != 3 || restored.HeaderFor("https://auth.example/") != "sid=ordinary" {
		t.Fatal("checkpoint restore merged partitioned and unpartitioned cookies")
	}
	unknown := ordinary
	unknown.Name = "unknown"
	unknown.Partitioned = true
	if err = NewTransport(s, transport.NewCookieJar(unknown)).ImportCookies(ctx); err == nil {
		t.Fatal("unknown partition silently became an unpartitioned cookie")
	}
}

func TestBrowserTransportUsesNativeIdentityHeaders(t *testing.T) {
	s, ctx := integrationSession(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ua": r.UserAgent(), "hints": r.Header.Get("Sec-CH-UA"), "language": r.Header.Get("Accept-Language"), "full": r.Header.Get("Sec-CH-UA-Full-Version")})
	}))
	defer server.Close()
	response, err := NewTransport(s, nil).Do(ctx, transport.Request{URL: server.URL, Headers: http.Header{"Sec-Ch-Ua": {"invented-browser"}, "Sec-Ch-Ua-Full-Version": {"invented-full-version"}, "Accept-Language": {"xx-invented"}}})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err = json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result["ua"] != s.Identity().UserAgent || strings.Contains(result["hints"], "invented") || strings.Contains(result["full"], "invented") || strings.Contains(result["language"], "invented") {
		t.Fatal("Go profile headers changed browser identity")
	}
	if !strings.Contains(result["language"], s.Identity().Language) {
		t.Fatal("navigator and HTTP request languages differ")
	}
}

func TestBrowserCookieHandoffRejectsDifferentDevice(t *testing.T) {
	s, ctx := integrationSession(t)
	jar := transport.NewCookieJar(transport.Cookie{Name: "oai-did", Value: "different-device", Domain: "auth.example", Path: "/", HostOnly: true})
	if err := NewTransport(s, jar).ImportCookies(ctx); err == nil || !strings.Contains(err.Error(), "BROWSER_DEVICE_MISMATCH") {
		t.Fatal("checkpoint identity silently replaced bound device")
	}
}
