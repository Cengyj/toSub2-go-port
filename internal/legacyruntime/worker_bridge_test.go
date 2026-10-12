package legacyruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

// This subprocess is a deterministic protocol fixture, not a JavaScript host.
// It isolates parent bridge behavior from the engine's independent JS fixtures.
func bridgeWorkerFixture(args []string, input io.Reader, output io.Writer) (bool, int) {
	mode := os.Getenv("TOSUB2_SCRIPT_BRIDGE_FIXTURE")
	if mode == "process-tree" && len(args) > 0 && args[0] == "--process-tree-fixture" {
		if len(args) != 4 {
			return true, 1
		}
		if args[1] == "child" {
			executable, err := os.Executable()
			if err != nil {
				return true, 1
			}
			grandchild := exec.Command(executable, "--process-tree-fixture", "grandchild", "", "")
			if err := grandchild.Start(); err != nil {
				return true, 1
			}
			data, _ := json.Marshal(map[string]any{"parent": json.Number(args[3]), "child": os.Getpid(), "grandchild": grandchild.Process.Pid})
			if err := os.WriteFile(args[2], data, 0600); err != nil {
				return true, 1
			}
		} else if args[1] != "grandchild" {
			return true, 1
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "" || len(args) == 0 || args[0] != "--auth-js-worker" {
		return false, 0
	}
	encode := json.NewEncoder(output)
	fail := func() (bool, int) {
		_ = encode.Encode(map[string]any{"type": "failure", "code": "FIXTURE_PROTOCOL_FAILED"})
		return true, 1
	}
	if len(args) != 3 {
		return fail()
	}
	info, err := os.Stat(args[2])
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		return fail()
	}
	data, err := os.ReadFile(args[2])
	var initial map[string]any
	if err != nil || json.Unmarshal(data, &initial) != nil {
		return fail()
	}
	if mode == "process-tree" {
		executable, err := os.Executable()
		if err != nil {
			return fail()
		}
		pidFile, _ := initial["pidFile"].(string)
		child := exec.Command(executable, "--process-tree-fixture", "child", pidFile, fmt.Sprint(os.Getpid()))
		if err := child.Start(); err != nil {
			return fail()
		}
		_, _ = io.Copy(io.Discard, input)
		return true, 0
	}
	if mode == "block" {
		_, _ = io.Copy(io.Discard, input)
		return true, 0
	}
	if mode == "malformed" {
		_, _ = io.WriteString(output, "fixture-private-invalid-json\n")
		_, _ = io.Copy(io.Discard, input)
		return true, 0
	}
	if mode == "nested-errors" {
		_ = encode.Encode(map[string]any{"type": "final", "result": map[string]any{"errors": []string{"fixture-private-cookie"}, "reportedErrors": []map[string]string{{"error": "fixture-private-token"}}}})
		_, _ = io.Copy(io.Discard, input)
		return true, 0
	}
	if mode == "exit-memory" {
		_, _ = io.WriteString(os.Stderr, "fixture-private-process-path-and-stack\n")
		return true, 73
	}
	if mode == "exit-other" {
		_, _ = io.WriteString(os.Stderr, "fixture-private-process-path-and-stack\n")
		return true, 74
	}
	if mode == "failure" {
		_ = encode.Encode(map[string]any{"type": "failure", "code": "EXECUTION_FAILED", "value": "fixture-private-token", "url": "https://fixture.invalid/private"})
		_, _ = io.Copy(io.Discard, input)
		return true, 0
	}
	decode := json.NewDecoder(input)
	page, _ := initial["url"].(string)
	if mode == "window-graph" {
		firstGraph := json.RawMessage(`{"items":[null,true,{"custom":[3,"unchanged"]}],"opaqueVersion":17}`)
		secondGraph := json.RawMessage(`{"items":[{"kind":"unsupported-by-parent"},[4,5]],"opaqueVersion":99}`)
		if args[1] == "child" {
			messages, _ := initial["parentMessages"].([]any)
			graphs, _ := initial["parentMessageGraphs"].([]any)
			if len(messages) != 1 || len(graphs) != 1 {
				return fail()
			}
			data, _ := json.Marshal(messages[0])
			graph, _ := json.Marshal(graphs[0])
			if string(data) != `{"event":"execute"}` || string(graph) != string(firstGraph) {
				return fail()
			}
			_ = encode.Encode(map[string]any{"type": "parent-message", "message": map[string]any{"data": map[string]any{"event": "complete"}, "graph": secondGraph}})
			_, _ = io.Copy(io.Discard, input)
			return true, 0
		}
		origin, err := url.Parse(page)
		if err != nil || args[1] != "parent" {
			return fail()
		}
		// Emitting the message before the frame forces the production owner
		// to preserve its opaque graph in the child's initial input queue.
		_ = encode.Encode(map[string]any{"type": "frame-message", "message": map[string]any{"data": map[string]any{"event": "execute"}, "graph": firstGraph}})
		_ = encode.Encode(map[string]any{"type": "frame", "url": origin.Scheme + "://" + origin.Host + "/frame/rch/graph-fixture"})
		var reply struct {
			Type  string
			Data  json.RawMessage
			Graph json.RawMessage
		}
		if decode.Decode(&reply) != nil || reply.Type != "child-message" || string(reply.Data) != `{"event":"complete"}` || string(reply.Graph) != string(secondGraph) {
			return fail()
		}
		_ = encode.Encode(map[string]any{"type": "cookie", "id": 1, "url": page, "action": "set", "value": "cf_clearance=graph-bridge-clearance; Path=/"})
		var cookieReply struct{ Type string }
		if decode.Decode(&cookieReply) != nil || cookieReply.Type != "cookie-result" {
			return fail()
		}
		_, _ = io.Copy(io.Discard, input)
		return true, 0
	}
	var nextID int
	cookie := func(action, value string) (string, bool) {
		nextID++
		if encode.Encode(map[string]any{"type": "cookie", "id": nextID, "url": page, "action": action, "value": value}) != nil {
			return "", false
		}
		var reply struct {
			Type, Value string
			ID          int
		}
		if decode.Decode(&reply) != nil || reply.Type != "cookie-result" || reply.ID != nextID {
			return "", false
		}
		return reply.Value, true
	}
	if mode == "challenge" {
		visible, ok := cookie("get", "")
		if !ok || strings.Contains(visible, "protected=") {
			return fail()
		}
		if _, ok = cookie("set", "cf_clearance=bridge-clearance; Path=/"); !ok {
			return fail()
		}
		_, _ = io.Copy(io.Discard, input)
		return true, 0
	}
	for {
		var call struct{ Type, Method, Flow string }
		if decode.Decode(&call) != nil {
			return true, 0
		}
		if call.Type != "call" {
			return fail()
		}
		visible, ok := cookie("get", "")
		if !ok || strings.Contains(visible, "protected=") || !strings.Contains(visible, "oai-did=fixture-device") {
			return fail()
		}
		if call.Method == "token" {
			for _, value := range []string{"public=script; Path=/", "protected=forged; Path=/", "new-private=forged; HttpOnly; Path=/"} {
				if _, ok = cookie("set", value); !ok {
					return fail()
				}
			}
			sdkURL, _ := initial["sdkUrl"].(string)
			origin, parseErr := url.Parse(sdkURL)
			if parseErr != nil {
				return fail()
			}
			requestURL := origin.Scheme + "://" + origin.Host + "/bridge-binary"
			_ = encode.Encode(map[string]any{"type": "request", "request": map[string]any{"id": 501, "kind": "fetch", "method": "POST", "url": requestURL, "body": "wrong-text-body", "bodyBase64": base64.StdEncoding.EncodeToString([]byte{0, 255, 128, 65}), "headers": map[string]string{"Cookie": "spoof=forbidden"}}})
			var reply struct {
				ID, Status int
				BodyBase64 string
			}
			if decode.Decode(&reply) != nil || reply.ID != 501 || reply.Status != 200 || reply.BodyBase64 != base64.StdEncoding.EncodeToString([]byte{255, 0, 1}) {
				return fail()
			}
			visible, ok = cookie("get", "")
			if !ok || !strings.Contains(visible, "public=server-next") || strings.Contains(visible, "server-private=") || strings.Contains(visible, "new-private=") {
				return fail()
			}
		} else if call.Method != "sessionObserverToken" || !strings.Contains(visible, "public=server-next") {
			return fail()
		}
		token := map[string]any{"c": "fixture-challenge", "id": "fixture-device", "flow": call.Flow}
		if call.Method == "token" {
			token["p"], token["t"] = "fixture-proof", nil
		} else {
			token["so"] = "fixture-observer"
		}
		value, _ := json.Marshal(token)
		if encode.Encode(map[string]any{"type": "result", "method": call.Method, "flow": call.Flow, "value": string(value)}) != nil {
			return fail()
		}
	}
}

func bridgeTestClient(t *testing.T, mode, loader string) (*Client, string) {
	t.Helper()
	t.Setenv("TOSUB2_SCRIPT_BRIDGE_FIXTURE", mode)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	c, err := New(context.Background(), Options{SDKURL: loader, SolverTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c, tmp
}

func TestGoWorkerBridgeSentinelKeepsAuthoritativeCookiesAndBinaryBytes(t *testing.T) {
	var writes atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loader.js":
			fmt.Fprintf(w, `var script=document.createElement("script");script.src=%q;document.head.appendChild(script);`, server.URL+"/sentinel/fixture/sdk.js")
		case "/sentinel/fixture/sdk.js":
			_, _ = io.WriteString(w, "fixture source is not evaluated by the protocol fixture")
		case "/bridge-binary":
			writes.Add(1)
			body, _ := io.ReadAll(r.Body)
			cookies := r.Header.Get("Cookie")
			if !bytes.Equal(body, []byte{0, 255, 128, 65}) || !strings.Contains(cookies, "protected=server") || !strings.Contains(cookies, "public=script") || strings.Contains(cookies, "forged") || strings.Contains(cookies, "spoof") {
				t.Error("worker bridge corrupted bytes or bypassed authoritative cookies")
			}
			w.Header().Add("Set-Cookie", "public=server-next; Path=/")
			w.Header().Add("Set-Cookie", "server-private=server; HttpOnly; Path=/")
			_, _ = w.Write([]byte{255, 0, 1})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, tmp := bridgeTestClient(t, "sentinel", server.URL+"/loader.js")
	c.cookieJar().SetFromResponse(server.URL, http.Header{"Set-Cookie": {"protected=server; HttpOnly; Path=/", "public=initial; Path=/"}})
	headers, err := c.Sentinel(context.Background(), server.URL, "fixture-device", "password_verify", server.URL+"/login/password")
	if err != nil || headers.Get("Openai-Sentinel-Token") == "" || headers.Get("Openai-Sentinel-So-Token") == "" || writes.Load() != 1 {
		t.Fatal("Go worker parent bridge did not complete both SDK calls", err)
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestGoWorkerBridgeChallengeUsesScriptClearanceAndHTTPVerification(t *testing.T) {
	var verifications atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && strings.Contains(r.Header.Get("Cookie"), "cf_clearance=bridge-clearance") {
			verifications.Add(1)
			_, _ = io.WriteString(w, "verified fixture")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<script>var _cf_chl_opt={};</script>`)
	}))
	defer server.Close()
	c, tmp := bridgeTestClient(t, "challenge", "")
	c.cookieJar().SetFromResponse(server.URL, http.Header{"Set-Cookie": {"protected=server; HttpOnly; Path=/"}})
	if _, err := c.Do(context.Background(), transport.Request{URL: server.URL + "/challenge"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Challenge(context.Background(), server.URL+"/challenge"); err != nil || verifications.Load() != 1 {
		t.Fatal("script cookie was not verified through the same HTTP session", err)
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestGoWorkerBridgeKeepsQueuedAndLiveWindowGraphsOpaque(t *testing.T) {
	var verifications atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/challenge":
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<script>var _cf_chl_opt={};</script>`)
		case "/frame/rch/graph-fixture":
			_, _ = io.WriteString(w, "<html>graph fixture</html>")
		case "/":
			verifications.Add(1)
			cookie, err := r.Cookie("cf_clearance")
			if err != nil || cookie.Value != "graph-bridge-clearance" {
				t.Error("opaque graph bridge lost its clearance")
			}
			_, _ = io.WriteString(w, "verified")
		default:
			t.Error("opaque graph bridge used an unexpected route")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, tmp := bridgeTestClient(t, "window-graph", "")
	if _, err := client.Do(t.Context(), transport.Request{URL: server.URL + "/challenge"}); err != nil {
		t.Fatal(err)
	}
	if err := client.Challenge(t.Context(), server.URL+"/challenge"); err != nil {
		t.Fatal("opaque graph was changed while queued or forwarded", err)
	}
	if verifications.Load() != 1 {
		t.Fatal("opaque graph fixture did not verify its clearance once")
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestGoWorkerBridgeFailureCodesAreClosedAndPrivateDataIsIgnored(t *testing.T) {
	for _, code := range []string{"", "bad-code", "PRIVATE https://fixture.invalid", "PRIVATE_ACCOUNT_SECRET_ALL_CAPS", strings.Repeat("A", 81)} {
		if err := runtimeFailure(runtimeMessage{Code: code}); !errors.Is(err, ErrProtocol) {
			t.Fatal("untrusted worker diagnostic code was displayed")
		}
	}
	if err := runtimeFailure(runtimeMessage{Code: "EXECUTION_FAILED"}); err.Error() != "GO_JS_RUNTIME: EXECUTION_FAILED" {
		t.Fatal("valid closed worker diagnostic was lost")
	}
	c, tmp := bridgeTestClient(t, "failure", "")
	c.challenges["https://fixture.invalid/"] = []byte(`<script>var _cf_chl_opt={};</script>`)
	err := c.Challenge(context.Background(), "https://fixture.invalid/")
	if err == nil || !strings.Contains(err.Error(), "GO_JS_RUNTIME: EXECUTION_FAILED") || strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), "fixture.invalid") {
		t.Fatal("worker failure was not safely surfaced", err)
	}
	assertRuntimeInputsRemoved(t, tmp)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "/sentinel/fixture/sdk.js")
	}))
	defer server.Close()
	c.opts.SDKURL = server.URL + "/loader.js"
	headers, err := c.Sentinel(context.Background(), server.URL, "fixture-device", "password_verify", server.URL+"/login/password")
	if headers != nil || err == nil || !strings.Contains(err.Error(), "GO_JS_RUNTIME: EXECUTION_FAILED") || strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), "fixture.invalid") {
		t.Fatal("Sentinel did not reject the same safe worker failure", err)
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestGoWorkerBridgeFixedExitPreservesOnlyKnownResourceDiagnosis(t *testing.T) {
	for _, mode := range []string{"exit-memory", "exit-other"} {
		for _, operation := range []string{"sentinel", "challenge"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				client, tmp := bridgeTestClient(t, mode, "")
				var err error
				if operation == "challenge" {
					client.challenges["https://fixture.invalid/"] = []byte(`<script>var _cf_chl_opt={};</script>`)
					err = client.Challenge(t.Context(), "https://fixture.invalid/")
				} else {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.WriteString(w, "/sentinel/fixture/sdk.js")
					}))
					defer server.Close()
					client.opts.SDKURL = server.URL + "/loader.js"
					_, err = client.Sentinel(t.Context(), server.URL, "fixture-device", "password_verify", server.URL+"/log-in/password")
				}
				if err == nil || strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), "exit status") || strings.Contains(err.Error(), tmp) {
					t.Fatal("worker exit surfaced a private process error")
				}
				if memory := strings.Contains(err.Error(), "GO_JS_RUNTIME: JS_RESOURCE_LIMIT_MEMORY"); memory != (mode == "exit-memory") {
					t.Fatal("fixed resource exit classification was lost or guessed", err)
				}
				assertRuntimeInputsRemoved(t, tmp)
			})
		}
	}
}

func TestGoWorkerBridgeMissingWorkerFailsClosed(t *testing.T) {
	c, tmp := bridgeTestClient(t, "block", "")
	c.opts.WorkerPath = filepath.Join(t.TempDir(), "missing-go-worker")
	events := make(chan runtimeEvent, 2)
	if runtime, err := c.startRuntime(context.Background(), "parent", map[string]any{}, events); runtime != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing Go worker did not fail closed", err)
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestGoWorkerBridgeCancellationReapsPrivateInput(t *testing.T) {
	c, tmp := bridgeTestClient(t, "block", "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	runtime, err := c.startRuntime(ctx, "parent", map[string]any{}, make(chan runtimeEvent, 2))
	if err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if err := runtime.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.done:
	default:
		t.Fatal("worker process survived canceled operation")
	}
	assertRuntimeInputsRemoved(t, tmp)
}

func TestRuntimeRequestRejectsNonCanonicalBase64BeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	c := nativeClient(t, Options{})
	for _, value := range []string{"!fixture-private!", "AAB=", "AA==\n", "AA"} {
		_, err := c.submitRuntimeRequest(context.Background(), server.URL, generatedRequest{ID: json.RawMessage("1"), Method: "POST", URL: server.URL, BodyBase64: &value}, true)
		if !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "fixture-private") {
			t.Fatal("invalid binary request was accepted or disclosed", err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid binary requests reached the network")
	}
}

func TestRuntimeRequestKeepsEmptyBinaryAndLegacyTextBodiesDistinct(t *testing.T) {
	bodies := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
	}))
	defer server.Close()
	c := nativeClient(t, Options{})
	empty := ""
	for _, request := range []generatedRequest{
		{ID: json.RawMessage("1"), Method: "POST", URL: server.URL, Body: "not-the-binary-body", BodyBase64: &empty},
		{ID: json.RawMessage("2"), Method: "POST", URL: server.URL, Body: "legacy-text-body"},
	} {
		if _, err := c.submitRuntimeRequest(context.Background(), server.URL, request, true); err != nil {
			t.Fatal(err)
		}
	}
	if <-bodies != "" || <-bodies != "legacy-text-body" {
		t.Fatal("empty binary body was replaced by legacy text or legacy body was lost")
	}
}

func TestRuntimeCookieRejectsUncorrelatedOrMalformedEvents(t *testing.T) {
	c := nativeClient(t, Options{})
	for _, message := range []runtimeMessage{
		{ID: json.RawMessage("0"), Action: "get", URL: "https://fixture.invalid/"},
		{ID: json.RawMessage("1.5"), Action: "get", URL: "https://fixture.invalid/"},
		{ID: json.RawMessage("9007199254740992"), Action: "get", URL: "https://fixture.invalid/"},
		{ID: json.RawMessage("1"), Action: "get", URL: "https://private:credential@fixture.invalid/"},
		{ID: json.RawMessage("1"), Action: "get", URL: "file:///fixture"},
		{ID: json.RawMessage("1"), Action: "set", URL: "https://fixture.invalid/"},
		{ID: json.RawMessage("1"), Action: "delete", URL: "https://fixture.invalid/"},
	} {
		if _, err := c.submitRuntimeCookie(message); !errors.Is(err, ErrProtocol) {
			t.Fatal("malformed cookie event mutated the session")
		}
	}
}
