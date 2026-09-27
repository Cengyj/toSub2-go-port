package challenge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
)

func TestChromiumSentinelWaitsForPublicDocumentChallenge(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium document challenge fixtures")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = browser.DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed")
	}
	for _, completes := range []bool{true, false} {
		t.Run(fmt.Sprint(completes), func(t *testing.T) {
			var pages, loaders atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/login":
					n := pages.Add(1)
					w.Header().Set("Content-Type", "text/html")
					if n == 1 || !completes {
						w.Header().Set("Cf-Mitigated", "challenge")
						w.WriteHeader(403)
						fmt.Fprint(w, `<!doctype html><div id=challenge-stage>provider gate</div>`)
						if completes {
							fmt.Fprint(w, `<script>setTimeout(()=>document.querySelector('#challenge-stage').remove(),120)</script>`)
						}
					} else {
						fmt.Fprint(w, `<!doctype html><input type=email>`)
					}
				case "/loader.js":
					loaders.Add(1)
					w.Header().Set("Content-Type", "application/javascript")
					fmt.Fprint(w, `window.SentinelSDK={token:async flow=>JSON.stringify({p:'fixture',t:'fixture',c:'fixture',id:'fixture',flow})}`)
				default:
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			manager := browser.NewManager(path, 1)
			defer manager.Close()
			s, err := manager.Start(ctx, "", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			timeout := 3 * time.Second
			if !completes {
				timeout = 500 * time.Millisecond
			}
			result, err := (BrowserSolver{Session: s, Timeout: timeout}).Sentinel(ctx, server.URL+"/login", server.URL+"/loader.js", "authorize_continue")
			if completes {
				if err != nil || !result.Solved || pages.Load() != 2 || loaders.Load() != 1 {
					t.Fatalf("normal document was not verified before SDK: err=%v pages=%d loader=%d", err, pages.Load(), loaders.Load())
				}
			} else if err == nil || !strings.Contains(err.Error(), "SENTINEL_PAGE_CHALLENGE") || !strings.Contains(err.Error(), "HTTP 403") || !errors.Is(err, context.DeadlineExceeded) || loaders.Load() != 0 {
				t.Fatalf("provider gate classified as loader or bypassed: err=%v loaders=%d", err, loaders.Load())
			}
		})
	}
}

func TestChromiumSentinelRefusedDocumentNeverLoadsSDK(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium document refusal fixtures")
	}
	path := os.Getenv("CHROME_PATH")
	if path == "" {
		path = browser.DefaultChrome()
	}
	if path == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	var refused atomic.Bool
	var pages, loaders atomic.Int32
	refused.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/login":
			pages.Add(1)
			if refused.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(403)
				fmt.Fprint(w, "{\"error\":\"refused\"}")
				return
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><input type=email>")
		case "/loader.js":
			loaders.Add(1)
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, "window.SentinelSDK={token:async flow=>JSON.stringify({p:'fixture',t:'fixture',c:'fixture',id:'fixture',flow})}")
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager := browser.NewManager(path, 1)
	defer manager.Close()
	session, err := manager.Start(ctx, "", "", "", "local-sentinel-fixture")
	if err != nil {
		t.Fatal(err)
	}
	solver := BrowserSolver{Session: session, Timeout: 3 * time.Second}
	result, err := solver.Sentinel(ctx, server.URL+"/login", server.URL+"/loader.js", "authorize_continue")
	if !errors.Is(err, ErrVerificationRejected) || result.Solved || pages.Load() != 1 || loaders.Load() != 0 {
		t.Fatalf("initial refusal executed SDK: result=%+v err=%v pages=%d loaders=%d", result, err, pages.Load(), loaders.Load())
	}
	refused.Store(false)
	if err = session.Navigate(ctx, server.URL+"/login"); err != nil {
		t.Fatal(err)
	}
	result, err = solver.Sentinel(ctx, server.URL+"/login", server.URL+"/loader.js", "authorize_continue")
	if err != nil || !result.Solved || pages.Load() != 2 || loaders.Load() != 1 {
		t.Fatalf("normal document failed SDK fixture: result=%+v err=%v pages=%d loaders=%d", result, err, pages.Load(), loaders.Load())
	}
	if _, err = session.Evaluate(ctx, "window.__tosub2SentinelRuntime.loadedAt=1"); err != nil {
		t.Fatal(err)
	}
	refused.Store(true)
	result, err = solver.Sentinel(ctx, server.URL+"/login", server.URL+"/loader.js", "authorize_continue")
	if !errors.Is(err, ErrVerificationRejected) || result.Solved || pages.Load() != 3 || loaders.Load() != 1 {
		t.Fatalf("runtime reload refusal executed SDK: result=%+v err=%v pages=%d loaders=%d", result, err, pages.Load(), loaders.Load())
	}
	if !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "DOM=clear") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("refusal diagnostic missing or unsafe: %v", err)
	}
}
