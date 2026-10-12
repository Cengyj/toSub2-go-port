package legacyruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func TestGoSentinelPureGoCancellationTimeoutAndSessionReuse(t *testing.T) {
	testSentinelCancellationTimeoutAndSessionReuse(t)
}

// Exercise the complete Sentinel parent/worker/HTTP bridge. Engine-only
// interruption tests cannot establish that the owning session reaps its child,
// deletes private inputs and stays usable after caller-only cancellation.
func testSentinelCancellationTimeoutAndSessionReuse(t *testing.T) {
	workloads := map[string]string{
		"http-wait":      `await fetch('/pending');`,
		"cpu-loop":       `while(true){}`,
		"microtask-loop": `await Promise.resolve().then(function spin(){return Promise.resolve().then(spin)});`,
	}
	for _, mode := range []string{"caller-cancel", "owner-cancel", "close", "solver-timeout"} {
		for _, workload := range []string{"http-wait", "cpu-loop", "microtask-loop"} {
			t.Run(mode+"/"+workload, func(t *testing.T) {
				tmp := t.TempDir()
				t.Setenv("TMPDIR", tmp)
				started, release := make(chan struct{}), make(chan struct{})
				defer close(release)
				var startOnce sync.Once
				var recovered atomic.Bool
				var server *httptest.Server
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/loader.js":
						fmt.Fprintf(w, `var s=document.createElement('script');s.src=%q;document.head.appendChild(s);`, server.URL+"/sentinel/fixture/sdk.js")
					case "/sentinel/fixture/sdk.js":
						body := `await fetch('/started');` + workloads[workload]
						if recovered.Load() {
							body = `const r=await fetch('/proof');const p=await r.text();`
						}
						fmt.Fprintf(w, `window.SentinelSDK={token:async flow=>{%s return JSON.stringify({p:'lifecycle-proof',t:null,c:'fixture',id:document.cookie.split('oai-did=')[1].split(';')[0],flow});},sessionObserverToken:async flow=>JSON.stringify({so:'lifecycle-observer',c:'fixture',id:'lifecycle-device',flow})};`, body)
					case "/started":
						startOnce.Do(func() { close(started) })
						_, _ = io.WriteString(w, "started")
					case "/pending":
						select {
						case <-r.Context().Done():
						case <-release:
						}
					case "/proof":
						if cookie, err := r.Cookie("retained-session"); err != nil || cookie.Value != "lifecycle-cookie" {
							t.Error("caller cancellation lost the retained cookie session")
						}
						_, _ = io.WriteString(w, "lifecycle-proof")
					default:
						t.Error("lifecycle SDK used an unexpected fixture route")
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(server.Close)
				owner, cancelOwner := context.WithCancel(t.Context())
				defer cancelOwner()
				client := goTestClientWithOwner(t, owner, server.URL+"/loader.js")
				client.SetJar(transport.NewCookieJar(transport.Cookie{Name: "retained-session", Value: "lifecycle-cookie", Domain: "127.0.0.1", Path: "/", HostOnly: true}))
				if mode == "solver-timeout" {
					client.opts.SolverTimeout = time.Second
				}
				caller, cancelCaller := context.WithCancel(t.Context())
				defer cancelCaller()
				result := make(chan error, 1)
				go func() {
					_, err := client.Sentinel(caller, server.URL, "lifecycle-device", "password_verify", server.URL+"/log-in/password")
					result <- err
				}()
				select {
				case <-started:
				case err := <-result:
					t.Fatalf("SDK ended before its lifecycle workload: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("SDK did not start its lifecycle workload")
				}
				startedAt := time.Now()
				switch mode {
				case "caller-cancel":
					cancelCaller()
				case "owner-cancel":
					cancelOwner()
				case "close":
					if err := client.Close(); err != nil {
						t.Fatal(err)
					}
				}
				want := error(context.Canceled)
				if mode == "solver-timeout" {
					want = context.DeadlineExceeded
				}
				select {
				case err := <-result:
					if !errors.Is(err, want) {
						t.Fatalf("lifecycle operation did not preserve cancellation: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("SDK operation survived cancellation or timeout")
				}
				if time.Since(startedAt) > 3*time.Second {
					t.Fatal("worker cleanup exceeded the cancellation budget")
				}
				assertRuntimeInputsRemoved(t, tmp)
				if mode == "caller-cancel" || mode == "solver-timeout" {
					recovered.Store(true)
					client.opts.SolverTimeout = 3 * time.Second
					headers, err := client.Sentinel(t.Context(), server.URL, "lifecycle-device", "password_verify", server.URL+"/log-in/password")
					if err != nil || !strings.Contains(headers.Get("Openai-Sentinel-Token"), "lifecycle-proof") {
						t.Fatal("caller-only interruption made the retained session unusable")
					}
					assertRuntimeInputsRemoved(t, tmp)
				}
			})
		}
	}
}

func TestGoSentinelPureGoConcurrentSessionIsolation(t *testing.T) {
	testSentinelConcurrentSessionIsolation(t, false)
}

func TestGoSentinelPureGoConcurrentOriginStorageIsolation(t *testing.T) {
	testSentinelConcurrentSessionIsolation(t, true)
}

func testSentinelConcurrentSessionIsolation(t *testing.T, storage bool) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	const sessionCount = 8
	var proofs atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loader.js":
			fmt.Fprintf(w, `var s=document.createElement('script');s.src=%q;document.head.appendChild(s);`, server.URL+"/sentinel/fixture/sdk.js")
		case "/sentinel/fixture/sdk.js":
			setStorage, readFlow := "", "globalThis.privateFlow"
			if storage {
				setStorage = `localStorage.setItem('private-flow',flow);sessionStorage.setItem('private-flow',flow);`
				readFlow = `(localStorage.getItem('private-flow')===sessionStorage.getItem('private-flow')?localStorage.getItem('private-flow'):'storage-crossed')`
			}
			fmt.Fprintf(w, `window.SentinelSDK={token:async flow=>{globalThis.privateDevice=document.cookie.split('oai-did=')[1].split(';')[0];globalThis.privateFlow=flow;%s await new Promise(resolve=>setTimeout(resolve,10));const r=await fetch('/proof?device='+encodeURIComponent(privateDevice)+'&flow='+encodeURIComponent(%s));const data=await r.json();document.cookie='js-session='+privateDevice+'; Path=/';return JSON.stringify({p:data.proof,t:null,c:'fixture',id:privateDevice,flow:%s});},sessionObserverToken:async flow=>JSON.stringify({so:%s,c:'fixture',id:globalThis.privateDevice,flow})};`, setStorage, readFlow, readFlow, readFlow)
		case "/proof":
			proofs.Add(1)
			device, flow := r.URL.Query().Get("device"), r.URL.Query().Get("flow")
			cookie, err := r.Cookie("private-session")
			if err != nil || cookie.Value != device || flow != "flow-"+device {
				t.Error("concurrent worker request crossed an authentication session")
			}
			http.SetCookie(w, &http.Cookie{Name: "server-session", Value: device, Path: "/", HttpOnly: true})
			_ = json.NewEncoder(w).Encode(map[string]any{"proof": device})
		default:
			t.Error("concurrent SDK used an unexpected fixture route")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	clients := make([]*Client, sessionCount)
	for i := range clients {
		clients[i] = goTestClient(t, server.URL+"/loader.js")
		device := fmt.Sprintf("device-%d", i)
		clients[i].SetJar(transport.NewCookieJar(transport.Cookie{Name: "private-session", Value: device, Domain: "127.0.0.1", Path: "/", HostOnly: true}))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var group sync.WaitGroup
	for i, client := range clients {
		group.Add(1)
		go func(i int, client *Client) {
			defer group.Done()
			device := fmt.Sprintf("device-%d", i)
			headers, err := client.Sentinel(ctx, server.URL, device, "flow-"+device, server.URL+"/log-in/password")
			if err != nil {
				t.Errorf("concurrent SDK failed: %v", err)
				return
			}
			var token, observer map[string]any
			if json.Unmarshal([]byte(headers.Get("Openai-Sentinel-Token")), &token) != nil || json.Unmarshal([]byte(headers.Get("Openai-Sentinel-So-Token")), &observer) != nil || token["p"] != device || token["id"] != device || token["flow"] != "flow-"+device || observer["id"] != device || observer["so"] != "flow-"+device {
				t.Error("concurrent workers crossed JS globals or origin storage")
			}
			jar := client.cookieJar()
			if jar.Value("private-session", server.URL) != device || jar.Value("server-session", server.URL) != device {
				t.Error("concurrent workers crossed authoritative cookie jars")
			}
			if jar.Value("js-session", server.URL) != device {
				t.Error("Go document cookie write did not reach its own session")
			}
		}(i, client)
	}
	group.Wait()
	if proofs.Load() != sessionCount {
		t.Fatal("concurrent session requests were skipped or replayed")
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestGoCloudflarePureGoCancellationTimeoutAndSessionReuse(t *testing.T) {
	testCloudflareCancellationTimeoutAndSessionReuse(t)
}

func testCloudflareCancellationTimeoutAndSessionReuse(t *testing.T) {
	for _, mode := range []string{"caller-cancel", "owner-cancel", "close", "solver-timeout"} {
		t.Run(mode, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			var startOnce sync.Once
			var recovered atomic.Bool
			var completions atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/challenge":
					w.Header().Set("Content-Type", "text/html")
					w.Header().Set("Cf-Mitigated", "challenge")
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprintf(w, `<script>var _cf_chl_opt={};var f=document.createElement('iframe');f.src=%q;document.body.appendChild(f);f.contentWindow.postMessage({event:'execute'},'*');window.addEventListener('message',async e=>{if(e.data&&e.data.event==='complete')await fetch('/finish');});</script>`, server.URL+"/frame/rch/lifecycle")
				case "/frame/rch/lifecycle":
					work := `await fetch('/pending');`
					if recovered.Load() {
						work = ""
					}
					fmt.Fprintf(w, `<script>window.addEventListener('message',async e=>{if(e.data&&e.data.event==='execute'){%s window.parent.postMessage({event:'complete'},'*');}});</script>`, work)
				case "/pending":
					if cookie, err := r.Cookie("retained-session"); err != nil || cookie.Value != "challenge-cookie" {
						t.Error("running challenge child lost its owning cookie session")
					}
					startOnce.Do(func() { close(started) })
					select {
					case <-r.Context().Done():
					case <-release:
					}
				case "/finish":
					completions.Add(1)
					if cookie, err := r.Cookie("retained-session"); err != nil || cookie.Value != "challenge-cookie" {
						t.Error("recovered challenge lost its owning cookie session")
					}
					http.SetCookie(w, &http.Cookie{Name: "cf_clearance", Value: "lifecycle-clearance", Path: "/", HttpOnly: true})
					_, _ = io.WriteString(w, "accepted")
				case "/":
					if cookie, err := r.Cookie("cf_clearance"); err != nil || cookie.Value != "lifecycle-clearance" {
						t.Error("recovered challenge did not verify a fresh clearance")
					}
					_, _ = io.WriteString(w, "verified")
				default:
					t.Error("challenge lifecycle used an unexpected fixture route")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			owner, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			client := goTestClientWithOwner(t, owner, "")
			client.SetJar(transport.NewCookieJar(transport.Cookie{Name: "retained-session", Value: "challenge-cookie", Domain: "127.0.0.1", Path: "/", HostOnly: true}))
			if mode == "solver-timeout" {
				client.opts.SolverTimeout = 2 * time.Second
			}
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			if response, err := client.Do(caller, transport.Request{URL: server.URL + "/challenge"}); err != nil || response.StatusCode != http.StatusForbidden {
				t.Fatal("could not obtain the lifecycle challenge")
			}
			result := make(chan error, 1)
			go func() { result <- client.Challenge(caller, server.URL+"/challenge") }()
			select {
			case <-started:
			case err := <-result:
				t.Fatalf("challenge ended before its running child request: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("challenge child did not start its HTTP request")
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 2 {
				t.Fatal("challenge did not own two private parent/child inputs")
			}
			switch mode {
			case "caller-cancel":
				cancelCaller()
			case "owner-cancel":
				cancelOwner()
			case "close":
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
			}
			want := error(context.Canceled)
			if mode == "solver-timeout" {
				want = context.DeadlineExceeded
			}
			select {
			case err := <-result:
				if !errors.Is(err, want) {
					t.Fatalf("challenge did not preserve lifecycle cancellation: %v", err)
				}
				if mode == "solver-timeout" && !strings.Contains(err.Error(), "solver time budget exhausted") {
					t.Fatal("challenge timeout lost its fixed solver-budget diagnosis")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("challenge parent/child survived cancellation")
			}
			assertRuntimeInputsRemoved(t, tmp)
			if completions.Load() != 0 {
				t.Fatal("canceled challenge issued a false successful clearance")
			}
			if mode == "caller-cancel" || mode == "solver-timeout" {
				recovered.Store(true)
				client.opts.SolverTimeout = 3 * time.Second
				if _, err := client.Do(t.Context(), transport.Request{URL: server.URL + "/challenge"}); err != nil {
					t.Fatal("interrupted challenge made its retained HTTP session unusable")
				}
				if err := client.Challenge(t.Context(), server.URL+"/challenge"); err != nil {
					t.Fatal("interrupted challenge could not solve a fresh challenge in the retained session", err)
				}
				if completions.Load() != 1 {
					t.Fatal("fresh challenge clearance was replayed")
				}
				assertRuntimeInputsRemoved(t, tmp)
			}
		})
	}
}
