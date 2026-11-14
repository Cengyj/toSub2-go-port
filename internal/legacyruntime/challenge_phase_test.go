package legacyruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tosub2-go-port/internal/jshost"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/transport"
)

// The same safe code can represent three different failure locations even when
// both resource requests returned 200. All resources here are local fixtures.
func TestChallengeFatalExecutionPhases(t *testing.T) {
	for _, phase := range []string{"initial_script", "dynamic_script", "timer_callback"} {
		t.Run(phase, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			var mu sync.Mutex
			calls := map[string]int{}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls[r.URL.Path]++
				mu.Unlock()
				switch r.URL.Path {
				case "/challenge":
					w.Header().Set("Content-Type", "text/html")
					w.Header().Set("Cf-Mitigated", "challenge")
					w.WriteHeader(http.StatusForbidden)
					// Script insertion queues a request synchronously, unlike fetch's
					// asynchronous body preparation. The read-only cookie round trip
					// therefore waits for both replies before the intentional failure.
					load := `function load(url){const script=document.createElement("script");script.src=url;document.head.appendChild(script);}`
					source := fmt.Sprintf(`%sload(%q);load(%q);void document.cookie;throw new Error("fixture-private-url-token-password");`, load, server.URL+"/first", server.URL+"/second")
					if phase == "dynamic_script" {
						source = fmt.Sprintf(`%sload(%q);`, load, server.URL+"/first")
					} else if phase == "timer_callback" {
						source = fmt.Sprintf(`%sload(%q);setTimeout(()=>{load(%q);void document.cookie;throw new Error("fixture-private-url-token-password");},0);`, load, server.URL+"/first", server.URL+"/second")
					}
					fmt.Fprintf(w, `<script>var _cf_chl_opt={};%s</script>`, source)
				case "/first":
					if phase == "dynamic_script" {
						fmt.Fprintf(w, `load(%q);void document.cookie;throw new Error("fixture-private-url-token-password");`, server.URL+"/second")
					} else {
						fmt.Fprint(w, `void 0;`)
					}
				case "/second":
					fmt.Fprint(w, `void 0;`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			c := goTestClient(t, "")
			c.opts.SolverTimeout = 3 * time.Second
			if _, err := c.Do(t.Context(), transport.Request{URL: server.URL + "/challenge"}); err != nil {
				t.Fatal(err)
			}
			err := c.Challenge(t.Context(), server.URL+"/challenge")
			var diagnostic *jshost.Error
			if err == nil || !errors.As(err, &diagnostic) || diagnostic.Code != "SCRIPT_EXCEPTION" || diagnostic.Phase != phase {
				t.Fatalf("fatal execution classification lost: %v", err)
			}
			if diagnostic.Location == nil || !jshost.ValidDiagnosticLocation(diagnostic.Location) {
				t.Fatal("process runtime lost its numeric exception location")
			}
			message := logging.RedactText(err.Error())
			for _, want := range []string{"challenge execution failed", "stage=parent_execution", "frame=false child=false", "requests=2 last_http=200", "parent_errors=1 child_errors=0", "phase=" + phase} {
				if !strings.Contains(message, want) {
					t.Fatalf("missing safe diagnostic %q: %v", want, err)
				}
			}
			if strings.Contains(message, "fixture-private") || strings.Contains(message, server.URL) {
				t.Fatal("private runtime details escaped")
			}
			// The cached response is consumed once, even when execution failed.
			if retry := c.Challenge(context.Background(), server.URL+"/challenge"); retry == nil || !strings.Contains(retry.Error(), "no matching challenge response") {
				t.Fatal("failed challenge could be resumed from old input")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != 3 || calls["/challenge"] != 1 || calls["/first"] != 1 || calls["/second"] != 1 {
				t.Fatalf("challenge repeated a request or added verification: %v", calls)
			}
			assertRuntimeInputsRemoved(t, tmp)
		})
	}
}

func TestRuntimeFailureRejectsUntrustedExecutionPhase(t *testing.T) {
	for _, phase := range []string{"fixture-private", "initial_script\nfixture-private", "https://fixture.invalid/private"} {
		err := runtimeFailure(runtimeMessage{Code: "SCRIPT_EXCEPTION", Phase: phase})
		if !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "fixture-private") {
			t.Fatal("untrusted runtime phase was accepted or exposed")
		}
	}
}

func TestChallengeFatalChildErrorCountsOnlyChild(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/challenge":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `<script>var _cf_chl_opt={};const f=document.createElement("iframe");f.src=%q;document.body.appendChild(f);</script>`, server.URL+"/child")
		case "/child":
			_, _ = w.Write([]byte(`<script>throw new Error("fixture-private-child");</script>`))
		default:
			t.Error("unexpected request after child script failure")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := goTestClient(t, "")
	c.opts.SolverTimeout = 3 * time.Second
	if _, err := c.Do(t.Context(), transport.Request{URL: server.URL + "/challenge"}); err != nil {
		t.Fatal(err)
	}
	err := c.Challenge(t.Context(), server.URL+"/challenge")
	if err == nil || !strings.Contains(err.Error(), "parent_errors=0 child_errors=1") || !strings.Contains(err.Error(), "phase=initial_script") || strings.Contains(err.Error(), "fixture-private") {
		t.Fatalf("child fatal error was lost or attributed to the parent: %v", err)
	}
	assertRuntimeInputsRemoved(t, tmp)
}
