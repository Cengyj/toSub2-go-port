package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCurrentDocumentDiagnosticRejectsStaleOrUnrelatedResponses(t *testing.T) {
	s := &Session{}
	const target = "https://example.test/private-path?token=private-query"
	emit := func(session, method string, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		s.recordResourceDiagnostic(cdpMessage{SessionID: session, Method: method, Params: raw})
	}
	response := func(id, session, frame, loader, kind string, status int, challenged bool) {
		headers := map[string]string{}
		if challenged {
			headers["Cf-Mitigated"] = "challenge"
		}
		emit(session, "Network.responseReceived", map[string]any{"requestId": id, "frameId": frame, "loaderId": loader, "type": kind, "response": map[string]any{"url": target, "status": status, "mimeType": "text/html", "headers": headers}})
	}
	response("main", "", "main-frame", "loader-one", "Document", 403, true)
	// The same address in fetches, child renderers and old navigation caches
	// does not establish the state of this page's current main document.
	response("fetch", "", "main-frame", "loader-one", "Fetch", 200, false)
	response("child", "", "child-frame", "loader-child", "Document", 200, false)
	response("attached", "other-renderer", "main-frame", "loader-one", "Document", 200, false)
	emit("", "Network.responseReceivedExtraInfo", map[string]any{"requestId": "main", "statusCode": 200, "headers": map[string]string{"Content-Type": "application/json"}})
	d, known := s.currentDocumentDiagnostic("main-frame", "loader-one", target+"#private-fragment")
	if !known || d.Status != 403 || !d.Challenge {
		t.Fatalf("document evidence replaced by unrelated request: %#v %v", d, known)
	}
	for _, loader := range []string{"", "new-unobserved-loader"} {
		if _, known := s.currentDocumentDiagnostic("main-frame", loader, target); known {
			t.Fatal("stale navigation treated as current")
		}
	}
	response("next", "", "main-frame", "loader-two", "Document", 200, false)
	d, known = s.currentDocumentDiagnostic("main-frame", "loader-two", target)
	if !known || d.Status != 200 || d.Challenge {
		t.Fatalf("new document not observed: %#v", d)
	}
	if _, known := s.currentDocumentDiagnostic("main-frame", "loader-two", "https://example.test/different"); known {
		t.Fatal("different URL reused evidence")
	}
	if _, known := s.currentDocumentDiagnostic("main-frame", "loader-two", "about:blank"); known {
		t.Fatal("non-HTTP page accepted")
	}
	if strings.Contains(fmt.Sprintf("%#v", s.diagnosticEntries), "private-") {
		t.Fatal("document evidence retained URL or query data")
	}
}

func TestChromiumCurrentDocumentDiagnosticTracksNavigationNotFetch(t *testing.T) {
	s, ctx := integrationSession(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/denied":
			if r.Header.Get("Sec-Fetch-Dest") != "document" {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, "{}")
				return
			}
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(403)
			fmt.Fprint(w, "<!doctype html><title>Access denied</title>")
		case "/redirect":
			http.Redirect(w, r, "/json", http.StatusFound)
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "{}")
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	if err := s.Navigate(ctx, server.URL+"/denied"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evaluate(ctx, "fetch('/denied').then(r=>r.status)"); err != nil {
		t.Fatal(err)
	}
	d, known := s.CurrentDocumentDiagnostic(ctx)
	if !known || d.Status != 403 || d.ContentKind != "html" {
		t.Fatalf("fetch replaced document: %#v known=%v", d, known)
	}
	if err := s.Navigate(ctx, server.URL+"/redirect"); err != nil {
		t.Fatal(err)
	}
	d, known = s.CurrentDocumentDiagnostic(ctx)
	if !known || d.Status != 200 || d.ContentKind != "json" {
		t.Fatalf("redirected main document missing: %#v known=%v", d, known)
	}
}

func TestCurrentDocumentEvidenceSurvivesResourceCacheEviction(t *testing.T) {
	for _, responseFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(responseFirst), func(t *testing.T) {
			s := &Session{}
			const target = "https://example.test/private-document?token=private-query"
			emit := func(session, method string, value any) {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				s.recordResourceDiagnostic(cdpMessage{SessionID: session, Method: method, Params: raw})
			}
			navigate := func(session, frame, parent, loader string) {
				emit(session, "Page.frameNavigated", map[string]any{"frame": map[string]string{"id": frame, "parentId": parent, "loaderId": loader, "url": target}})
			}
			response := func(id, frame, loader, kind string) {
				emit("", "Network.responseReceived", map[string]any{"requestId": id, "frameId": frame, "loaderId": loader, "type": kind, "response": map[string]any{"url": target, "status": 200, "mimeType": "text/html"}})
			}
			if responseFirst {
				response("main", "main-frame", "one", "Document")
			}
			navigate("", "main-frame", "", "one")
			if !responseFirst {
				response("main", "main-frame", "one", "Document")
			}
			for i := 0; i < 300; i++ {
				response(fmt.Sprint("fetch-", i), "main-frame", "one", "Fetch")
				child := fmt.Sprint("child-", i)
				response(child, child, child, "Document")
				navigate("", child, "main-frame", child)
				navigate("attached-target", "main-frame", "", child)
			}
			if _, cached := s.diagnosticEntries["/main"]; cached {
				t.Fatal("fixture did not evict old resource")
			}
			if len(s.diagnosticEntries) != 256 {
				t.Fatal("resource cache became unbounded")
			}
			d, known := s.currentDocumentDiagnostic("main-frame", "one", target)
			if !known || d.Status != 200 {
				t.Fatalf("current document was evicted: %#v known=%v", d, known)
			}
			emit("", "Network.loadingFailed", map[string]string{"requestId": "main", "errorText": "net::ERR_CONNECTION_RESET"})
			d, known = s.currentDocumentDiagnostic("main-frame", "one", target)
			if !known || d.NetworkError != "net::ERR_CONNECTION_RESET" {
				t.Fatal("late document failure was lost after cache eviction")
			}
			if strings.Contains(fmt.Sprintf("%#v", s.diagnosticDocument), "private-") {
				t.Fatal("pinned document retained sensitive URL")
			}
			navigate("", "main-frame", "", "two")
			if _, known := s.currentDocumentDiagnostic("main-frame", "two", target); known {
				t.Fatal("new document reused old status")
			}
			response("new-main", "main-frame", "two", "Document")
			d, known = s.currentDocumentDiagnostic("main-frame", "two", target)
			if !known || d.Status != 200 || d.NetworkError != "" {
				t.Fatalf("new document retained old failure: %#v", d)
			}
		})
	}
}
