package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/mail"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func TestRandomStrongPassword(t *testing.T) {
	a, e := randomStrongPassword()
	if e != nil {
		t.Fatal(e)
	}
	b, e := randomStrongPassword()
	if e != nil {
		t.Fatal(e)
	}
	if a == b || len(a) < 24 || !strings.ContainsAny(a, "!@#$%") || !strings.ContainsAny(a, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") || !strings.ContainsAny(a, "0123456789") {
		t.Fatal("weak/reused password")
	}
}
func TestRefreshPreservesUnknownAndProxyFields(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "job", "sub2api-import-oauth.json")
	jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"fixture@example.com","sid":"account"}`)) + ".x"
	raw := map[string]any{"type": "sub2api-data", "version": 1, "custom": "keep", "proxies": []any{map[string]any{"name": "proxy"}}, "accounts": []any{map[string]any{"name": "fixture", "extra": map[string]any{"custom": "preserve"}, "proxy_id": 42, "credentials": map[string]any{"email": "fixture@example.com", "access_token": "old", "refresh_token": "refresh-old", "id_token": jwt, "custom": "also-keep"}}}}
	if e := protocol.WritePrivateJSON(path, raw); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh-old" {
			t.Error("wrong refresh token")
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "new"})
	}))
	defer server.Close()
	if e := refreshExistingExport(context.Background(), transport.NewClient(nil), server.URL, "client", root, "job", ""); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	var got map[string]any
	if e := json.Unmarshal(b, &got); e != nil {
		t.Fatal(e)
	}
	account := got["accounts"].([]any)[0].(map[string]any)
	cred := account["credentials"].(map[string]any)
	if got["custom"] != "keep" || len(got["proxies"].([]any)) != 1 || account["proxy_id"] != float64(42) || cred["custom"] != "also-keep" || cred["access_token"] != "new" || cred["refresh_token"] != "refresh-old" || account["extra"].(map[string]any)["custom"] != "preserve" {
		t.Fatal("refresh dropped compatible fields")
	}
}
func TestMailManualOverrideCancelsSlowFetch(t *testing.T) {
	requested := make(chan struct{})
	canceled := make(chan struct{})
	var ready atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(requested); <-r.Context().Done(); close(canceled) }))
	defer s.Close()
	result := make(chan string, 1)
	go func() {
		v, e := pollMailInput(context.Background(), config.Config{MailPollTimeoutMS: 2000}, s.URL, mail.RequestConfig{}, 0, newMailInputState(), func() (string, bool) { return "123456", ready.Load() }, nil)
		if e != nil {
			result <- e.Error()
		} else {
			result <- v
		}
	}()
	<-requested
	ready.Store(true)
	select {
	case v := <-result:
		if v != "123456" {
			t.Fatal(v)
		}
	case <-time.After(time.Second):
		t.Fatal("manual override blocked by fetch")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("fetch not canceled")
	}
}
func TestMailDoesNotResubmitRejectedCode(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"code":"123456"}`)) }))
	defer s.Close()
	state := newMailInputState()
	if e := state.baseline(context.Background(), s.URL, mail.RequestConfig{}, nil); e != nil {
		t.Fatal(e)
	}
	_, e := pollMailInput(context.Background(), config.Config{MailPollTimeoutMS: 150}, s.URL, mail.RequestConfig{}, 0, state, func() (string, bool) { return "", false }, nil)
	if e == nil || !strings.Contains(e.Error(), "MAIL_OTP_TIMEOUT") {
		t.Fatalf("reused old OTP: %v", e)
	}
}
