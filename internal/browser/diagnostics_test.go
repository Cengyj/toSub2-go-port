package browser

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestResourceDiagnosticBoundedAndCredentialFree(t *testing.T) {
	s := &Session{}
	emit := func(method string, value any) {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		s.recordResourceDiagnostic(cdpMessage{Method: method, Params: b})
	}
	for i := 0; i < 300; i++ {
		id := fmt.Sprint(i)
		emit("Network.requestWillBeSent", map[string]any{"requestId": id, "request": map[string]any{"url": "https://private-user:private-pass@example.test/private-path?api_key=private-query", "postData": "private-body", "headers": map[string]string{"Cookie": "private-cookie"}}})
		emit("Network.responseReceivedExtraInfo", map[string]any{"requestId": id, "statusCode": 403, "headers": map[string]string{"Content-Type": "text/html; private-mime-parameter", "Cf-Mitigated": "challenge", "Set-Cookie": "private-cookie", "Authorization": "private-token"}})
		emit("Network.loadingFailed", map[string]any{"requestId": id, "errorText": "net::ERR_BLOCKED_BY_ORB", "blockedReason": "csp"})
	}
	d := s.ResourceDiagnostic("https://private-user:private-pass@example.test/private-path?api_key=private-query")
	if d.Status != 403 || d.ContentKind != "html" || !d.Challenge || d.BlockedReason != "csp" || d.NetworkError != "net::ERR_BLOCKED_BY_ORB" {
		t.Fatalf("diagnostic missing expected classifications: %#v", d)
	}
	if len(s.diagnosticEntries) != 256 || len(s.diagnosticOrder) != 256 {
		t.Fatal("diagnostics retain unbounded requests")
	}
	if strings.Contains(fmt.Sprintf("%#v %s", s.diagnosticEntries, d.Summary()), "private-") {
		t.Fatal("diagnostics retained credentials or URL/body content")
	}
	if diagnosticCode("provider echoed private-token with spaces") != "" {
		t.Fatal("arbitrary remote text accepted as a browser code")
	}
	if diagnosticCode("net::ERR_HTTP2_PROTOCOL_ERROR") == "" {
		t.Fatal("valid numeric Chromium error code discarded")
	}
}

func TestResourceDiagnosticMatchesOriginalRedirectedLoader(t *testing.T) {
	s := &Session{}
	for _, target := range []string{"https://sdk.example.test/loader.js", "https://sdk.example.test/version/sdk.js"} {
		b, _ := json.Marshal(map[string]any{"requestId": "one", "request": map[string]string{"url": target}})
		s.recordResourceDiagnostic(cdpMessage{Method: "Network.requestWillBeSent", Params: b})
	}
	b, _ := json.Marshal(map[string]any{"requestId": "one", "response": map[string]any{"url": "https://sdk.example.test/version/sdk.js", "status": 404, "mimeType": "text/html"}})
	s.recordResourceDiagnostic(cdpMessage{Method: "Network.responseReceived", Params: b})
	if s.ResourceDiagnostic("https://sdk.example.test/loader.js").Status != 404 {
		t.Fatal("redirected loader diagnosis disappeared")
	}
}
