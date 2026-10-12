package jshost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Audit fixtures use isolated script/network protocols and synthetic replies.
// No remote SDK, cookies, account credentials or authentication service is used.
func auditRequests(t *testing.T, output *bytes.Buffer) []command {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var requests []command
	for decoder.More() {
		var event command
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != "request" {
			t.Fatalf("unexpected fixture event type %v", event["type"])
		}
		request, ok := event["request"].(map[string]any)
		if !ok {
			t.Fatal("request fixture has no request record")
		}
		requests = append(requests, command(request))
	}
	output.Reset()
	return requests
}

func auditReply(t *testing.T, h *host, request command, body string) {
	t.Helper()
	if err := h.receive(inputEvent{command: command{"id": request["id"], "status": float64(200), "body": body, "headers": map[string]any{"content-type": "text/plain"}}}); err != nil {
		t.Fatal(err)
	}
	drainTasks(t, h)
}

func TestAuditXHRAbortInStateCallbackDiscardsOldResponse(t *testing.T) {
	for _, abortState := range []int{2, 3} {
		t.Run(fmt.Sprintf("state-%d", abortState), func(t *testing.T) {
			h, r, output := testRealm(t, "https://fixture.test/")
			evalTest(t, r, fmt.Sprintf(`
var events=[],handled=false,xhr=new XMLHttpRequest();
xhr.open('GET','/old');
xhr.onreadystatechange=()=>{events.push('state:'+xhr.readyState);if(!handled&&xhr.readyState===%d){handled=true;xhr.abort();}};
xhr.onabort=()=>events.push('abort');xhr.onload=()=>events.push('load');xhr.onloadend=()=>events.push('loadend');xhr.send();`, abortState))
			requests := auditRequests(t, output)
			if len(requests) != 1 {
				t.Fatal("fixture did not start exactly one request")
			}
			auditReply(t, h, requests[0], "old-response-must-be-discarded")
			got := evalTest(t, r, `JSON.stringify({state:xhr.readyState,status:xhr.status,text:xhr.responseText,response:xhr.response,loads:events.filter(e=>e==='load').length,aborts:events.filter(e=>e==='abort').length,ends:events.filter(e=>e==='loadend').length})`).String()
			want := `{"state":0,"status":0,"text":"","response":null,"loads":0,"aborts":1,"ends":1}`
			if got != want {
				t.Fatalf("aborted XHR retained an old response: %s", got)
			}
			if len(auditRequests(t, output)) != 0 {
				t.Fatal("aborted response unexpectedly started another request")
			}
		})
	}
}

func TestAuditXHROpenInStateCallbackDiscardsOldResponse(t *testing.T) {
	for _, openState := range []int{2, 3} {
		t.Run(fmt.Sprintf("state-%d", openState), func(t *testing.T) {
			h, r, output := testRealm(t, "https://fixture.test/")
			evalTest(t, r, fmt.Sprintf(`
var loads=[],ends=0,reopened=false,xhr=new XMLHttpRequest();xhr.open('GET','/old');
xhr.onreadystatechange=()=>{if(!reopened&&xhr.readyState===%d){reopened=true;xhr.open('GET','/new');xhr.send();}};
xhr.onload=()=>loads.push(xhr.responseText);xhr.onloadend=()=>ends++;xhr.send();`, openState))
			requests := auditRequests(t, output)
			if len(requests) != 1 {
				t.Fatal("fixture did not start exactly one request")
			}
			auditReply(t, h, requests[0], "old-response-must-be-discarded")
			newRequests := auditRequests(t, output)
			if len(newRequests) != 1 || newRequests[0]["url"] != "https://fixture.test/new" {
				t.Fatal("reentrant open did not start the new request")
			}
			if got := evalTest(t, r, `JSON.stringify({state:xhr.readyState,text:xhr.responseText,loads,ends})`).String(); got != `{"state":1,"text":"","loads":[],"ends":0}` {
				t.Fatalf("old response continued after open: %s", got)
			}
			auditReply(t, h, newRequests[0], "new-response")
			if got := evalTest(t, r, `JSON.stringify({state:xhr.readyState,text:xhr.responseText,loads,ends})`).String(); got != `{"state":4,"text":"new-response","loads":["new-response"],"ends":1}` {
				t.Fatalf("new response did not complete once: %s", got)
			}
		})
	}
}

func TestAuditWorkersSelfCloseReleaseActiveSlots(t *testing.T) {
	value := runSDKFixture(t, `SentinelSDK={token:()=>new Promise((resolve,reject)=>{
  const url=URL.createObjectURL(new Blob(['onmessage=()=>{postMessage("closed");close();}']));
  let count=0;
  function next(){
    if(count===33){URL.revokeObjectURL(url);resolve(String(count));return;}
    try{
      const worker=new Worker(url);
      worker.onerror=e=>reject(new Error(e.message));
      worker.onmessage=e=>{if(e.data!=='closed'){reject(new Error('unexpected worker message'));return;}count++;setTimeout(next,5);};
      worker.postMessage('start');
    }catch(error){reject(error);}
  }
  next();
})};`, nil)
	if value != "33" {
		t.Fatal("self-closed worker slots were not reusable")
	}
}

func TestAuditFetchRequestCannotBeConsumedTwice(t *testing.T) {
	requests := 0
	value := runSDKFixture(t, `SentinelSDK={token:async()=>{
  const request=new Request('/consume',{method:'POST',body:'one-payload'});
  const first=await fetch(request);await first.text();
  let rejected=false;try{await fetch(request);}catch(error){rejected=error instanceof TypeError;}
  return JSON.stringify({used:request.bodyUsed,rejected});
}};`, func(request command) command {
		requests++
		if request["method"] != "POST" || request["bodyBase64"] != "b25lLXBheWxvYWQ=" {
			t.Fatal("Request body was corrupted")
		}
		return command{"status": 200, "body": "response"}
	})
	if value != `{"used":true,"rejected":true}` || requests != 1 {
		t.Fatalf("fetch repeated an already consumed request: value=%s, requests=%d", value, requests)
	}
}

func TestAuditFetchLockedRequestWaitsForReaderRelease(t *testing.T) {
	requests := 0
	value := runSDKFixture(t, `SentinelSDK={token:async()=>{
  const request=new Request('/locked',{method:'POST',body:'one-payload'}),reader=request.body.getReader();
  let rejected=false;try{await fetch(request);}catch(error){rejected=error instanceof TypeError;}
  const unused=request.bodyUsed===false&&request.body.locked===true;reader.releaseLock();
  const response=await fetch(request);return JSON.stringify({rejected,unused,text:await response.text(),used:request.bodyUsed});
}};`, func(request command) command {
		requests++
		return command{"status": 200, "body": "accepted"}
	})
	if value != `{"rejected":true,"unused":true,"text":"accepted","used":true}` || requests != 1 {
		t.Fatalf("locked request was consumed or could not resume: value=%s, requests=%d", value, requests)
	}
}

func TestAuditFetchAndXHRHideResponseCookieHeaders(t *testing.T) {
	value := runSDKFixture(t, `SentinelSDK={token:async()=>{
  const response=await fetch('/headers');
  const fetched={cookie:response.headers.get('set-cookie'),cookie2:response.headers.get('SET-COOKIE2'),has:response.headers.has('set-cookie'),listed:[...response.headers].some(([name])=>/^set-cookie2?$/.test(name)),visible:response.headers.get('x-fixture')};
  const xhr=await new Promise((resolve,reject)=>{const x=new XMLHttpRequest();x.open('GET','/headers');x.onload=()=>resolve(x);x.onerror=()=>reject(new Error('XHR failed'));x.send();});
  return JSON.stringify({fetched,xhr:{cookie:xhr.getResponseHeader('Set-Cookie'),cookie2:xhr.getResponseHeader('set-cookie2'),listed:/set-cookie/i.test(xhr.getAllResponseHeaders()),visible:xhr.getResponseHeader('x-fixture')}});
}};`, func(request command) command {
		return command{"status": 200, "body": "fixture", "headers": map[string]any{"Set-Cookie": "synthetic-secret=value; HttpOnly", "set-cookie2": "synthetic-obsolete=value", "X-Fixture": "visible"}}
	})
	want := `{"fetched":{"cookie":null,"cookie2":null,"has":false,"listed":false,"visible":"visible"},"xhr":{"cookie":null,"cookie2":null,"listed":false,"visible":"visible"}}`
	if value != want {
		t.Fatal("a response cookie header was exposed to JavaScript", value)
	}
}

func TestAuditLoaderIsVisibleAndCurrentScriptClears(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var output bytes.Buffer
	h := &host{ctx: ctx, kind: "sentinel", input: map[string]any{
		"url": "https://fixture.test/page", "loaderUrl": "https://fixture.test/loader.js", "sdkUrl": "https://fixture.test/sdk.js",
		"loaderSource": `let loaderLexical=40;var loaderGlobal=42;globalThis.loaderState={src:document.currentScript.src,connected:document.currentScript.isConnected,listed:Array.from(document.scripts).includes(document.currentScript),count:document.scripts.length};queueMicrotask(()=>{globalThis.loaderMicrotaskClear=document.currentScript===null});const script=document.createElement('script');script.src='https://fixture.test/sdk.js';script.onload=()=>{globalThis.sdkLoadClear=document.currentScript===null;globalThis.sdkLoadGlobals=[loaderLexical,loaderGlobal,sdkLexical,sdkGlobal]};document.head.appendChild(script);`,
		"sdkSource":    `let sdkLexical=loaderLexical+1;var sdkGlobal=loaderGlobal+1;globalThis.sdkState={src:document.currentScript.src,connected:document.currentScript.isConnected,listed:Array.from(document.scripts).includes(document.currentScript),count:document.scripts.length};queueMicrotask(()=>{globalThis.sdkMicrotaskClear=document.currentScript===null});`,
	}, writer: json.NewEncoder(&output), pending: map[int64]func(command) error{}, started: time.Now()}
	t.Cleanup(func() {
		for _, r := range h.realms {
			r.stopInterrupt()
			r.vm.Interrupt("TEST_DONE")
		}
	})
	if err := h.initialize(); err != nil {
		t.Fatal(err)
	}
	if got := evalTest(t, h.root, `JSON.stringify({state:loaderState,clear:document.currentScript===null,microtaskClear:loaderMicrotaskClear,globals:[loaderLexical,loaderGlobal],varProperty:globalThis.loaderGlobal})`).String(); got != `{"state":{"src":"https://fixture.test/loader.js","connected":true,"listed":true,"count":1},"clear":true,"microtaskClear":true,"globals":[40,42],"varProperty":42}` {
		t.Fatalf("loader currentScript lifetime or script list is wrong: %s", got)
	}
	drainTasks(t, h)
	if got := evalTest(t, h.root, `JSON.stringify({state:sdkState,clear:document.currentScript===null,microtaskClear:sdkMicrotaskClear,scripts:Array.from(document.scripts,s=>s.src),loadClear:sdkLoadClear,loadGlobals:sdkLoadGlobals,globals:[loaderLexical,loaderGlobal,sdkLexical,sdkGlobal],varProperties:[globalThis.loaderGlobal,globalThis.sdkGlobal]})`).String(); got != `{"state":{"src":"https://fixture.test/sdk.js","connected":true,"listed":true,"count":2},"clear":true,"microtaskClear":true,"scripts":["https://fixture.test/loader.js","https://fixture.test/sdk.js"],"loadClear":true,"loadGlobals":[40,42,41,43],"globals":[40,42,41,43],"varProperties":[42,43]}` {
		t.Fatalf("SDK currentScript lifetime or script list is wrong: %s", got)
	}
	if output.Len() != 0 || len(h.pending) != 0 {
		t.Fatal("cached loader or SDK was requested from the network again")
	}
}

func TestAuditInitialInlineScriptsHaveDOMIdentityAndGlobalBindings(t *testing.T) {
	for _, kind := range []string{"parent", "child"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			var output bytes.Buffer
			h := &host{ctx: ctx, kind: kind, input: map[string]any{
				"url": "https://fixture.test/page", "parentUrl": "https://parent.fixture.test/",
				"html": `<!doctype html><html><head>
<script id="data" type="application/json">{"fixture":true}</script>
<script id="first">let inlineLexical=20;var inlineGlobal=22;globalThis.inlineStates=[{id:document.currentScript.id,src:document.currentScript.src,listed:Array.from(document.scripts).includes(document.currentScript),connected:document.currentScript.isConnected,count:document.scripts.length}];queueMicrotask(()=>{globalThis.firstMicrotaskClear=document.currentScript===null});</script>
<script id="second" type="text/javascript">inlineStates.push({id:document.currentScript.id,src:document.currentScript.src,listed:Array.from(document.scripts).includes(document.currentScript),connected:document.currentScript.isConnected,count:document.scripts.length});globalThis.inlineGlobals=[inlineLexical,inlineGlobal];queueMicrotask(()=>{globalThis.secondMicrotaskClear=document.currentScript===null});</script>
</head><body></body></html>`,
			}, writer: json.NewEncoder(&output), pending: map[int64]func(command) error{}, started: time.Now()}
			t.Cleanup(func() {
				for _, r := range h.realms {
					r.stopInterrupt()
					r.vm.Interrupt("TEST_DONE")
				}
			})
			if err := h.initialize(); err != nil {
				t.Fatal(err)
			}
			got := evalTest(t, h.root, `JSON.stringify({states:inlineStates,clear:document.currentScript===null,microtasks:[firstMicrotaskClear,secondMicrotaskClear],globals:inlineGlobals,laterGlobals:[inlineLexical,inlineGlobal],varProperty:globalThis.inlineGlobal,data:JSON.parse(document.getElementById('data').textContent).fixture})`).String()
			want := `{"states":[{"id":"first","src":"","listed":true,"connected":true,"count":3},{"id":"second","src":"","listed":true,"connected":true,"count":3}],"clear":true,"microtasks":[true,true],"globals":[20,22],"laterGlobals":[20,22],"varProperty":22,"data":true}`
			if got != want {
				t.Fatalf("initial inline script identity or execution scope is wrong: %s", got)
			}
			if kind == "child" && !evalTest(t, h.root, `parent!==window&&parent===top`).ToBoolean() {
				t.Fatal("child fixture lost its external parent")
			}
			if output.Len() != 0 || len(h.pending) != 0 {
				t.Fatal("initial inline/data scripts unexpectedly used the network")
			}
		})
	}
}

func TestAuditDynamicInlineExecutesSynchronouslyOnceAndSkipsData(t *testing.T) {
	_, r, output := testRealm(t, "https://fixture.test/page")
	evalTest(t, r, `var injected=document.createElement('script');injected.textContent="var dynamicRuns=(globalThis.dynamicRuns||0)+1;globalThis.dynamicScriptIdentity=document.currentScript===injected;queueMicrotask(()=>{globalThis.dynamicMicrotaskClear=document.currentScript===null});";document.head.appendChild(injected);var synchronous=dynamicRuns===1;document.head.removeChild(injected);document.body.appendChild(injected);var dataScript=document.createElement('script');dataScript.type='application/json';dataScript.textContent='{"fixture":true}';document.head.appendChild(dataScript);`)
	got := evalTest(t, r, `JSON.stringify({runs:dynamicRuns,synchronous,identity:dynamicScriptIdentity,clear:document.currentScript===null,microtaskClear:dynamicMicrotaskClear,data:JSON.parse(dataScript.textContent).fixture,count:document.scripts.length})`).String()
	want := `{"runs":1,"synchronous":true,"identity":true,"clear":true,"microtaskClear":true,"data":true,"count":2}`
	if got != want {
		t.Fatalf("dynamic inline execution did not follow its node lifecycle: %s", got)
	}
	if output.Len() != 0 {
		t.Fatal("dynamic inline/data scripts unexpectedly used the network")
	}
}
