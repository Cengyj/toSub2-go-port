package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/settings"
)

func TestIntegrationSettingsPrivateScopeAndExplicitCommit(t *testing.T) {
	p, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{token: "test", integrationSettings: settings.New(t.TempDir(), p)}
	path := "/api/integration-settings/sub2api"
	for _, token := range []string{"", "wrong"} {
		w := frontendRequest(s, http.MethodGet, path, nil, token)
		if w.Code != http.StatusForbidden || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("private read was not protected", w.Code)
		}
	}
	request := map[string]any{"revision": 0, "config": map[string]any{"adminApiKey": "synthetic-private", "empty": []string{}, "enabled": false, "zero": 0}}
	w := frontendRequest(s, http.MethodPut, path, request, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var receipt struct {
		Revision          int64
		Verified, Applied bool
	}
	if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.Revision != 1 || !receipt.Verified || receipt.Applied {
		t.Fatal("incorrect persistence/application receipt", w.Body.String())
	}
	w = frontendRequest(s, http.MethodPut, path, request, "test")
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "synthetic-private") {
		t.Fatal("stale browser overwrite or secret in error", w.Body.String())
	}
	w = frontendRequest(s, http.MethodPost, path+"/confirm", map[string]any{"revision": 1}, "test")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"verified":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// A forged Host cannot obtain secrets, and its rejection remains no-store.
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "rebinding.invalid"
	r.Header.Set("X-Console-Token", "test")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("origin error lost private response headers")
	}
}

func TestProtectedSettingsStaleApplicationCannotOverwriteProcess(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	protector, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	s.integrationSettings = settings.New(t.TempDir(), protector)
	for _, scope := range []settings.Scope{settings.Mail, settings.Sub2API} {
		if _, err := s.integrationSettings.Save(context.Background(), scope, 0, json.RawMessage(`{"generation":1}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.integrationSettings.Save(context.Background(), scope, 1, json.RawMessage(`{"generation":2}`)); err != nil {
			t.Fatal(err)
		}
	}
	mail := map[string]any{"protectedRevision": 2, "config": map[string]any{"method": "POST", "url": "https://mail.example.test/current"}}
	w := frontendRequest(s, http.MethodPost, "/api/mail-request-config", mail, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	mail["protectedRevision"] = 1
	mail["config"] = map[string]any{"method": "GET", "url": "https://mail.example.test/stale"}
	w = frontendRequest(s, http.MethodPost, "/api/mail-request-config", mail, "test")
	if w.Code != http.StatusConflict || s.mail.URL != "https://mail.example.test/current" || s.mail.Method != "POST" {
		t.Fatal("stale mail configuration applied", w.Code)
	}
	monitor := map[string]any{"protectedRevision": 2, "baseUrl": "https://sub.example.test", "adminApiKey": "synthetic-current", "enabled": false}
	w = frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", monitor, "test")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	monitor["protectedRevision"], monitor["adminApiKey"] = 1, "synthetic-stale"
	w = frontendRequest(s, http.MethodPost, "/api/sub2api/monitor", monitor, "test")
	if w.Code != http.StatusConflict || s.monitorConfig.APIKey != "synthetic-current" || strings.Contains(w.Body.String(), "synthetic-") {
		t.Fatal("stale Sub2API configuration applied", w.Code)
	}
}

func TestIntegrationSettingsRejectsUnavailableProtectionAndAmbiguousJSON(t *testing.T) {
	s := &Server{token: "test", integrationSettings: settings.New(t.TempDir(), nil)}
	w := frontendRequest(s, http.MethodPut, "/api/integration-settings/mail", map[string]any{"revision": 0, "config": map[string]any{}}, "test")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "SETTINGS_UNAVAILABLE") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, raw := range []string{`{"revision":0,"revision":1,"config":{}}`, `{"revision":0,"config":{"key":1,"key":2}}`, `{"config":{}}`} {
		r := httptest.NewRequest(http.MethodPut, "/api/integration-settings/mail", strings.NewReader(raw))
		r.Host = "127.0.0.1"
		r.Header.Set("X-Console-Token", "test")
		w = httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatal("ambiguous update admitted", w.Code)
		}
	}
}
