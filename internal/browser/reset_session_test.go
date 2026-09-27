package browser

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestBrowserSessionResetClearsExpiredAuthAndKeepsDevice(t *testing.T) {
	session, ctx := integrationSession(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>Auth fixture</title>"))
	}))
	defer srv.Close()
	jar := transport.NewCookieJar(transport.Cookie{Name: "oai-did", Value: "fixture-device", Domain: "127.0.0.1", Path: "/", HostOnly: true}, transport.Cookie{Name: "auth_session", Value: "expired-auth", Domain: "127.0.0.1", Path: "/", HostOnly: true, HTTPOnly: true})
	client := NewTransport(session, jar)
	if err := client.ImportCookies(ctx); err != nil {
		t.Fatal(err)
	}
	if err := session.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Evaluate(ctx, "window.oldSDKState=true;document.cookie='browser_only=expired;path=/';true"); err != nil {
		t.Fatal(err)
	}
	profileDir, identity, device := session.Dir, session.Identity(), session.DeviceID
	if err := client.ResetSession(ctx); err != nil {
		t.Fatal(err)
	}
	if session.Dir != profileDir || session.Identity() != identity || session.DeviceID != device {
		t.Fatal("reset changed browser identity")
	}
	cookies, err := session.Cookies()
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 1 || cookies[0]["name"] != "oai-did" || cookies[0]["value"] != "fixture-device" {
		t.Fatal("browser kept expired authentication cookies or lost device")
	}
	if jar.Has("auth_session") || jar.Has("browser_only") || jar.Value("oai-did", srv.URL) != "fixture-device" {
		t.Fatal("reset jar drifted from browser")
	}
	if got, err := session.Evaluate(ctx, "typeof window.oldSDKState"); err != nil || got != "undefined" {
		t.Fatal("expired SDK page survived reset")
	}
}
