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
	"sync/atomic"
	"testing"
	"time"
	"tosub2-go-port/internal/transport"
)

func goTestClient(t *testing.T, loader string) *Client {
	return goTestClientWithOwner(t, context.Background(), loader)
}

func goTestClientWithOwner(t *testing.T, owner context.Context, loader string) *Client {
	t.Helper()
	c, err := New(owner, Options{SDKURL: loader, Profile: "chrome133", Timeout: 3 * time.Second, SolverTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func assertRuntimeInputsRemoved(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("private script input survived operation: %d files", len(entries))
	}
}

func TestGoSentinelPureGoSharesCookiesAndBrowserIdentity(t *testing.T) {
	testSentinelSharesCookiesAndBrowserIdentity(t)
}

func testSentinelSharesCookiesAndBrowserIdentity(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var calls, binaryCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loader.js":
			fmt.Fprintf(w, `var s=document.createElement("script");s.src=%q;document.head.appendChild(s);`, server.URL+"/sentinel/fixture/sdk.js")
		case "/sentinel/fixture/sdk.js":
			fmt.Fprintf(w, `window.SentinelSDK={token:async function(flow){var binary=await fetch(%q);var bytes=Array.from(new Uint8Array(await binary.clone().arrayBuffer())).join(",");var xhr=new XMLHttpRequest();xhr.open("GET",%q);xhr.responseType="arraybuffer";await new Promise(resolve=>{xhr.onload=resolve;xhr.send();});var xhrBytes=Array.from(new Uint8Array(xhr.response)).join(",");var r=await fetch(%q,{method:"POST",body:flow,headers:{"X-Generated":"fixture","X-Bytes":bytes,"X-XHR-Bytes":xhrBytes}});var c=await r.text();return JSON.stringify({p:"dynamic-proof",t:null,c:c,id:document.cookie.split("oai-did=")[1].split(";")[0],flow:flow,ua:navigator.userAgent,platform:navigator.platform});},sessionObserverToken:async function(flow){return JSON.stringify({so:"observer",c:"fixture",id:"fixture-device",flow:flow});}};`, server.URL+"/binary", server.URL+"/binary", server.URL+"/backend-api/sentinel/req")
		case "/binary":
			binaryCalls.Add(1)
			w.Header().Set("Content-Type", "application/wasm")
			_, _ = w.Write([]byte{0, 255, 128, 65})
		case "/backend-api/sentinel/req":
			calls.Add(1)
			body, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || string(body) != "password_verify" || r.Header.Get("X-Generated") != "fixture" || r.Header.Get("Origin") != server.URL || r.Header.Get("Sec-Fetch-Site") != "same-origin" || r.Header.Get("Content-Type") != "text/plain;charset=UTF-8" || !strings.Contains(r.Header.Get("Cookie"), "restored=shared") || !strings.Contains(r.Header.Get("Cookie"), "oai-did=fixture-device") {
				t.Error("Go runtime request lost headers, body or shared cookies")
			}
			if r.Header.Get("X-Bytes") != "0,255,128,65" || r.Header.Get("X-XHR-Bytes") != "0,255,128,65" {
				t.Error("Sentinel fetch/clone/XHR corrupted binary bytes")
			}
			w.Header().Add("Set-Cookie", "sdk-cookie=accepted; Path=/; HttpOnly")
			_, _ = w.Write([]byte("fixture-challenge"))
		default:
			t.Errorf("unexpected SDK path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := goTestClient(t, server.URL+"/loader.js")
	jar := transport.NewCookieJar(transport.Cookie{Name: "restored", Value: "shared", Domain: "127.0.0.1", Path: "/", HostOnly: true})
	c.SetJar(jar)
	h, err := c.Sentinel(context.Background(), server.URL, "fixture-device", "password_verify", server.URL+"/log-in/password")
	if err != nil {
		t.Fatal(err)
	}
	var token map[string]any
	if json.Unmarshal([]byte(h.Get("Openai-Sentinel-Token")), &token) != nil || token["id"] != "fixture-device" || token["flow"] != "password_verify" || token["c"] != "fixture-challenge" || token["ua"] != c.UserAgent() || token["platform"] != identity(c.Profile())["platform"] {
		t.Fatal("dynamic token or navigator identity changed")
	}
	if h.Get("Openai-Sentinel-So-Token") == "" || calls.Load() != 1 || binaryCalls.Load() != 2 || jar.Value("sdk-cookie", server.URL) != "accepted" {
		t.Fatal("observer or shared cookie session was lost")
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestGoSentinelInvalidSDKAndTokensFailClosed(t *testing.T) {
	for _, mode := range []string{"sdk-cross-origin", "sdk-redirect-cross-origin", "incomplete-token", "runtime-error", "empty-observer"} {
		t.Run(mode, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			var crossCalls atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { crossCalls.Add(1); _, _ = w.Write([]byte("unexpected")) }))
			defer other.Close()
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/loader.js":
					target := server.URL + "/sentinel/fixture/sdk.js"
					if mode == "sdk-cross-origin" {
						target = other.URL + "/sentinel/fixture/sdk.js"
					}
					fmt.Fprintf(w, `var s=document.createElement("script");s.src=%q;document.head.appendChild(s);`, target)
				case "/sentinel/fixture/sdk.js":
					if mode == "sdk-redirect-cross-origin" {
						http.Redirect(w, r, other.URL+"/sentinel/fixture/sdk.js", 302)
						return
					}
					if mode == "runtime-error" {
						_, _ = w.Write([]byte(`throw new Error("fixture-private-runtime-echo");`))
						return
					}
					token := `JSON.stringify({p:"proof",t:null,c:"fixture",id:"fixture-device",flow:flow})`
					if mode == "incomplete-token" {
						token = `JSON.stringify({id:"fixture-device",flow:flow})`
					}
					fmt.Fprintf(w, `window.SentinelSDK={token:function(flow){return %s;},sessionObserverToken:function(){return null;}};`, token)
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			c := goTestClient(t, server.URL+"/loader.js")
			if mode == "runtime-error" {
				c.opts.SolverTimeout = time.Second
			}
			h, err := c.Sentinel(context.Background(), server.URL, "fixture-device", "password_verify", server.URL+"/log-in/password")
			if mode == "empty-observer" {
				if err != nil || h.Get("Openai-Sentinel-Token") == "" || h.Get("Openai-Sentinel-So-Token") != "" {
					t.Fatal("optional empty observer no longer allowed", err)
				}
			} else if err == nil || strings.Contains(err.Error(), "fixture-private") || h != nil {
				t.Fatal("invalid SDK/token was accepted or disclosed remote errors", err)
			}
			if crossCalls.Load() != 0 {
				t.Fatal("foreign SDK origin was fetched")
			}
			assertRuntimeInputsRemoved(t, tmp)
		})
	}
}

func TestGoCloudflarePureGoParentChildBinaryAndClearance(t *testing.T) {
	testCloudflareParentChildBinaryAndClearance(t)
}

func testCloudflareParentChildBinaryAndClearance(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var frameCalls, binaryCalls, parentBinaryCalls, finishCalls, verifyCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/challenge":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(403)
			fmt.Fprintf(w, `<html><body><script>var _cf_chl_opt={};var frame=document.createElement("iframe");frame.src=%q;document.body.appendChild(frame);for(var event of ["init","extraParams","execute"]){frame.contentWindow.postMessage({event:event,widgetId:"fixture"},"*");}window.addEventListener("message",async function(e){if(e.data&&e.data.event==="complete"){var r=await fetch(%q);var bytes=Array.from(new Uint8Array(await r.clone().arrayBuffer())).join(",");var xhr=new XMLHttpRequest();xhr.open("GET",%q);xhr.responseType="arraybuffer";await new Promise(resolve=>{xhr.onload=resolve;xhr.send();});var xhrBytes=Array.from(new Uint8Array(xhr.response)).join(",");await fetch(%q,{method:"POST",body:e.data.bytes+":"+bytes+":"+xhrBytes});}});</script></body></html>`, server.URL+"/frame/rch/fixture", server.URL+"/parent-binary", server.URL+"/parent-binary", server.URL+"/finish")
		case "/frame/rch/fixture":
			frameCalls.Add(1)
			if r.Header.Get("Sec-Fetch-Dest") != "iframe" {
				t.Error("frame request lost navigation headers")
			}
			fmt.Fprintf(w, `<html><body><script>window.addEventListener("message",async function(e){if(e.data&&e.data.event==="execute"){var r=await fetch(%q);var bytes=Array.from(new Uint8Array(await r.arrayBuffer())).join(",");window.parent.postMessage({event:"complete",bytes:bytes},"*");}});</script></body></html>`, server.URL+"/binary")
		case "/binary":
			binaryCalls.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0, 255, 128, 65})
		case "/parent-binary":
			parentBinaryCalls.Add(1)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte{255, 0, 65})
		case "/finish":
			finishCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			if string(body) != "0,255,128,65:255,0,65:255,0,65" || r.Method != "POST" {
				t.Error("binary response or child-to-parent data corrupted")
			}
			w.Header().Add("Set-Cookie", "cf_clearance=fresh-fixture; Path=/; HttpOnly")
			_, _ = w.Write([]byte("accepted"))
		case "/":
			verifyCalls.Add(1)
			if !strings.Contains(r.Header.Get("Cookie"), "cf_clearance=fresh-fixture") {
				t.Error("verification lost clearance")
			}
			_, _ = w.Write([]byte("verified page"))
		default:
			t.Errorf("unexpected challenge path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := goTestClient(t, "")
	response, err := c.Do(context.Background(), transport.Request{URL: server.URL + "/challenge"})
	if err != nil || response.StatusCode != 403 {
		t.Fatal(err)
	}
	if err = c.Challenge(context.Background(), server.URL+"/challenge"); err != nil {
		t.Fatal(err)
	}
	if frameCalls.Load() != 1 || binaryCalls.Load() != 1 || parentBinaryCalls.Load() != 2 || finishCalls.Load() != 1 || verifyCalls.Load() != 1 {
		t.Fatalf("frame=%d binary=%d parentBinary=%d finish=%d verify=%d", frameCalls.Load(), binaryCalls.Load(), parentBinaryCalls.Load(), finishCalls.Load(), verifyCalls.Load())
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestGoCloudflareRejectsStaleClearanceAndFailedVerification(t *testing.T) {
	for _, mode := range []string{"stale", "verification-403", "verification-challenge"} {
		t.Run(mode, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			var verification atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/challenge":
					w.Header().Set("Content-Type", "text/html")
					w.Header().Set("Cf-Mitigated", "challenge")
					w.WriteHeader(403)
					fmt.Fprintf(w, `<script>var _cf_chl_opt={};fetch(%q,{method:"POST",body:"fixture"});</script>`, server.URL+"/complete")
				case "/complete":
					value := "new"
					if mode == "stale" {
						value = "old"
					}
					w.Header().Set("Set-Cookie", "cf_clearance="+value+"; Path=/")
					_, _ = w.Write([]byte("done"))
				case "/":
					verification.Add(1)
					if mode == "verification-403" {
						w.WriteHeader(403)
					} else {
						_, _ = w.Write([]byte("<script>var _cf_chl_opt={};</script>"))
					}
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			c := goTestClient(t, "")
			c.opts.SolverTimeout = 1500 * time.Millisecond
			c.cookieJar().Set(transport.Cookie{Name: "cf_clearance", Value: "old", Domain: "127.0.0.1", Path: "/", HostOnly: true})
			if _, err := c.Do(context.Background(), transport.Request{URL: server.URL + "/challenge"}); err != nil {
				t.Fatal(err)
			}
			if err := c.Challenge(context.Background(), server.URL+"/challenge"); err == nil {
				t.Fatal("challenge reported false success")
			}
			if mode == "stale" && verification.Load() != 0 {
				t.Fatal("old clearance was considered freshly issued")
			}
			assertRuntimeInputsRemoved(t, tmp)
		})
	}
}

func TestRuntimeInvalidNDJSONAndCancelledInputCleanup(t *testing.T) {
	for _, mode := range []string{"malformed", "block"} {
		t.Run(mode, func(t *testing.T) {
			c, tmp := bridgeTestClient(t, mode, "")
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			events := make(chan runtimeEvent, 2)
			r, err := c.startRuntime(ctx, "parent", map[string]string{"secret": "fixture-only"}, events)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "malformed" {
				select {
				case event := <-events:
					if !errors.Is(event.err, ErrProtocol) || strings.Contains(event.err.Error(), "fixture-private") {
						t.Fatal("invalid NDJSON was accepted or leaked", event.err)
					}
				case <-ctx.Done():
					t.Fatal("runtime did not reject malformed output")
				}
			} else {
				<-ctx.Done()
			}
			if err = r.close(); err != nil {
				t.Fatal(err)
			}
			assertRuntimeInputsRemoved(t, tmp)
		})
	}
}

func TestSentinelOwnerCancellationAndCloseReapRunningWorker(t *testing.T) {
	for _, mode := range []string{"owner-cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			requested := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/loader.js":
					fmt.Fprintf(w, `var s=document.createElement("script");s.src=%q;document.head.appendChild(s);`, server.URL+"/sentinel/fixture/sdk.js")
				case "/sentinel/fixture/sdk.js":
					fmt.Fprintf(w, `window.SentinelSDK={token:async function(flow){var r=await fetch(%q);return await r.text();},sessionObserverToken:function(){return null;}};`, server.URL+"/pending")
				case "/pending":
					close(requested)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				default:
					w.WriteHeader(404)
				}
			}))
			t.Cleanup(server.Close)
			owner, cancelOwner := context.WithCancel(context.Background())
			defer cancelOwner()
			c := goTestClientWithOwner(t, owner, server.URL+"/loader.js")
			result := make(chan error, 1)
			go func() {
				_, err := c.Sentinel(context.Background(), server.URL, "fixture-device", "password_verify", server.URL+"/log-in/password")
				result <- err
			}()
			select {
			case <-requested:
			case <-time.After(3 * time.Second):
				t.Fatal("Go SDK fixture did not start its HTTP request")
			}
			inputs, err := os.ReadDir(tmp)
			if err != nil || len(inputs) != 1 {
				t.Fatal("private runtime input not owned during script call", err)
			}
			info, err := inputs[0].Info()
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("runtime input is not private", err)
			}
			started := time.Now()
			if mode == "owner-cancel" {
				cancelOwner()
			} else if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("script operation did not preserve owner cancellation", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("script operation did not exit after owner cancellation/Close")
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("script cleanup exceeded cancellation budget")
			}
			assertRuntimeInputsRemoved(t, tmp)
		})
	}
}
