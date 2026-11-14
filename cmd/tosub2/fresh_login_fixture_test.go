package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// serveFreshLoginFixture drives the real normal password/callback login on the
// local mock. Every invocation obtains its own cookies from HTTP responses.
func serveFreshLoginFixture(t *testing.T, w http.ResponseWriter, r *http.Request, email string) bool {
	t.Helper()
	base := "http://" + r.Host
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/":
		if r.URL.Query().Get("action") != "" {
			return false
		}
		http.SetCookie(w, &http.Cookie{Name: "oai-did", Value: "fixture-device", Path: "/"})
		fmt.Fprint(w, "<html>fresh login</html>")
	case "/api/auth/providers":
		write(map[string]any{})
	case "/api/auth/csrf":
		write(map[string]any{"csrfToken": "fixture-csrf"})
	case "/api/auth/signin/openai":
		if _, err := r.Cookie("audit-session-token"); err == nil {
			t.Error("new login reused a prior operation cookie")
		}
		if r.URL.Query().Get("login_hint") != email {
			t.Error("fresh login changed account")
		}
		write(map[string]any{"url": base + "/log-in/password"})
	case "/log-in/password":
		fmt.Fprint(w, "<html>password</html>")
	case "/fixture-sdk.js":
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprintf(w, "const s=document.createElement('script');s.src=%q;document.head.appendChild(s);", base+"/sentinel/fixture/sdk.js")
	case "/sentinel/fixture/sdk.js":
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, `window.SentinelSDK={token:async flow=>JSON.stringify({p:'fixture',t:null,c:'fixture',id:'fixture-device',flow}),sessionObserverToken:async flow=>JSON.stringify({so:'fixture',c:'fixture',id:'fixture-device',flow})};`)
	case "/api/accounts/password/verify":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["password"] == "" {
			t.Error("fresh login bypassed password")
		}
		write(map[string]any{"continue_url": base + "/api/auth/callback/openai?code=fixture-once"})
	case "/api/auth/callback/openai":
		http.SetCookie(w, &http.Cookie{Name: "audit-session-token", Value: sessionAuditCookie, Path: "/", HttpOnly: true})
		http.SetCookie(w, &http.Cookie{Name: "audit-session-rotation", Value: sessionAuditCookie, Path: "/", HttpOnly: true})
		http.Redirect(w, r, base+"/", http.StatusFound)
	default:
		return false
	}
	return true
}
