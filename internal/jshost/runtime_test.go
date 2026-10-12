package jshost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/sobek"
)

func testRealm(t *testing.T, rawURL string) (*host, *realm, *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	out := new(bytes.Buffer)
	h := &host{ctx: ctx, kind: "sentinel", input: map[string]any{}, writer: json.NewEncoder(out), pending: map[int64]func(command) error{}, started: time.Now()}
	r, err := h.newRealm(map[string]any{"url": rawURL, "html": "<!doctype html><html><head></head><body></body></html>", "userAgent": "Fixture/1.0"}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	h.root = r
	t.Cleanup(func() {
		cancel()
		for _, realm := range h.realms {
			realm.stopInterrupt()
			realm.vm.Interrupt("TEST_DONE")
		}
	})
	return h, r, out
}

func evalTest(t *testing.T, r *realm, source string) sobek.Value {
	t.Helper()
	value, err := r.vm.RunString(source)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func drainTasks(t *testing.T, h *host) {
	t.Helper()
	for len(h.tasks) > 0 {
		if err := h.ctx.Err(); err != nil {
			t.Fatal(err)
		}
		index := 0
		for i, task := range h.tasks {
			if task.at.Before(h.tasks[index].at) {
				index = i
			}
		}
		task := h.tasks[index]
		h.tasks = append(h.tasks[:index], h.tasks[index+1:]...)
		if delay := time.Until(task.at); delay > 0 {
			time.Sleep(delay)
		}
		if task.repeat > 0 {
			t.Fatal("test unexpectedly left a repeating timer")
		}
		if err := task.run(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowMessageOriginsIdentityAndClone(t *testing.T) {
	h, r, _ := testRealm(t, "https://parent.example/path")
	evalTest(t, r, `var received=[];var frame=document.createElement('iframe');frame.src='https://child.example/';document.body.appendChild(frame);var child=frame.contentWindow;if(child!==frame.contentWindow)throw Error('unstable window');addEventListener('message',e=>received.push({data:e.data,source:e.source===child,origin:e.origin,trusted:e.isTrusted}));`)
	child := r.frames[r.vm.Get("frame").ToObject(r.vm)]
	evalTest(t, child, `addEventListener('message',e=>{if(e.source!==parent||e.origin!=='https://parent.example'||!e.isTrusted)throw Error('message identity');if(e.data.self!==e.data||e.data.view.buffer!==e.data.buffer||e.data.view[0]!==7)throw Error('clone graph');parent.postMessage({ok:true},'https://parent.example');});`)
	evalTest(t, r, `var buffer=new ArrayBuffer(3),view=new Uint8Array(buffer);view[0]=7;var value={buffer,view};value.self=value;child.postMessage(value,{targetOrigin:'https://child.example'});view[0]=9;child.postMessage('discard','https://unrelated.example');`)
	drainTasks(t, h)
	got := evalTest(t, r, `JSON.stringify(received)`).String()
	want := `[{"data":{"ok":true},"source":true,"origin":"https://child.example","trusted":true}]`
	if got != want {
		t.Fatalf("message=%s", got)
	}
	if !evalTest(t, r, `(()=>{try{child.document;return false}catch(e){return e.message.includes('SecurityError')}})()`).ToBoolean() {
		t.Fatal("cross-origin document was exposed")
	}
}

func TestSameOriginWindowMembrane(t *testing.T) {
	h, r, _ := testRealm(t, "https://same.example/page")
	evalTest(t, r, `var f=document.createElement('iframe');f.src='/frame';document.body.appendChild(f);var w=f.contentWindow;var childBody=w.document.body;childBody.setAttribute('data-test','ok');if(childBody!==w.document.body||w.parent!==window||w.top!==window)throw Error('membrane identity');`)
	child := r.frames[r.vm.Get("f").ToObject(r.vm)]
	evalTest(t, r, `if(!(w.document.body instanceof w.Node))throw Error('cross-realm instanceof');var array=new w.Array(1,2,3);if(array.length!==3||array.join(',')!=='1,2,3')throw Error('cross-realm constructor');if(Object.getPrototypeOf(w.document.body)!==w.HTMLBodyElement?.prototype && !(w.document.body instanceof w.HTMLElement))throw Error('cross-realm prototype');`)
	if got := evalTest(t, child, `document.body.getAttribute('data-test')`).String(); got != "ok" {
		t.Fatal(got)
	}
	evalTest(t, r, `w.addEventListener('message',e=>{window.localMessage=e.data;});w.postMessage('yes','/');`)
	drainTasks(t, h)
	if got := evalTest(t, r, "localMessage").String(); got != "yes" {
		t.Fatal(got)
	}
}

func TestWindowTransferAndNormalizedDefaultPortOrigin(t *testing.T) {
	h, r, _ := testRealm(t, "https://EXAMPLE.test:443/page")
	evalTest(t, r, `var transferred=false;addEventListener('message',e=>{
      if(e.origin!=='https://example.test'||e.source!==window||e.data.bytes[0]!==17||e.data.self!==e.data)throw Error('window transfer');
      transferred=true;
    });var bytes=new Uint8Array([17]),data={bytes};data.self=data;
    window.postMessage(data,'https://example.test',[bytes.buffer]);
    if(bytes.byteLength!==0)throw Error('not detached');
    var frame=document.createElement('iframe');frame.src='https://example.test/child';document.body.appendChild(frame);
    frame.contentWindow.document.body.id='same-origin';`)
	drainTasks(t, h)
	if !evalTest(t, r, "transferred").ToBoolean() {
		t.Fatal("normalized origin message not delivered")
	}
}

func TestTimersAndMicrotasks(t *testing.T) {
	h, r, _ := testRealm(t, "https://example.test/")
	evalTest(t, r, `var order=[];setTimeout(()=>{order.push('timer');queueMicrotask(()=>order.push('timer-micro'));},0);queueMicrotask(()=>order.push('micro'));Promise.resolve().then(()=>order.push('promise'));let id=setTimeout(()=>order.push('canceled'),0);clearTimeout(id);order.push('sync');`)
	drainTasks(t, h)
	if got := evalTest(t, r, `order.join(',')`).String(); got != "sync,micro,promise,timer,timer-micro" {
		t.Fatal(got)
	}
}

func TestFrameNavigationKeepsProxyAndRetiresOldTasks(t *testing.T) {
	h, r, _ := testRealm(t, "https://same.example/page")
	evalTest(t, r, `var frame=document.createElement('iframe');frame.src='/one';document.body.appendChild(frame);var proxy=frame.contentWindow;proxy.document.body.id='old';proxy.setTimeout(()=>{throw Error('old document timer fired')},0);proxy.postMessage('old','*');frame.src='/two';if(proxy!==frame.contentWindow||proxy.location.pathname!=='/two'||proxy.document.body.id)throw Error('navigation state');frame.src='https://other.example/';if(proxy!==frame.contentWindow||frame.contentDocument!==null)throw Error('navigation origin');`)
	drainTasks(t, h)
	if len(h.realms) != 4 {
		t.Fatal("navigation did not create fresh contexts")
	}
	for _, old := range h.realms[1:3] {
		if !old.retired {
			t.Fatal("old frame remained active")
		}
	}
}

func TestFetchAndXHRUseTransportBytes(t *testing.T) {
	h, r, out := testRealm(t, "https://example.test/base")
	evalTest(t, r, `var result, fetchError;fetch('/binary',{method:'POST',headers:{'X-Fixture':'yes'},body:new Uint8Array([0,128,255])}).then(async response=>{const copy=response.clone();result={status:response.status,url:response.url,bytes:Array.from(new Uint8Array(await response.arrayBuffer())),copy:Array.from(new Uint8Array(await copy.arrayBuffer()))};}).catch(e=>{fetchError=String(e)});`)
	var message struct{ Request map[string]any }
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &message); err != nil {
		t.Fatal(err)
	}
	if message.Request["bodyBase64"] != "AID/" || message.Request["sourceUrl"] != "https://example.test/base" {
		t.Fatalf("wrong request metadata: %v", message.Request)
	}
	if err := h.receive(inputEvent{command: command{"id": message.Request["id"], "status": float64(200), "url": "https://example.test/final", "bodyBase64": base64.StdEncoding.EncodeToString([]byte{255, 0, 128}), "headers": map[string]any{"content-type": "application/octet-stream"}}}); err != nil {
		t.Fatal(err)
	}
	drainTasks(t, h)
	if got := evalTest(t, r, `JSON.stringify(result)`).String(); got != `{"status":200,"url":"https://example.test/final","bytes":[255,0,128],"copy":[255,0,128]}` {
		t.Fatal(got, evalTest(t, r, `fetchError`).String())
	}
	out.Reset()
	evalTest(t, r, `var states=[],xhrResult;var xhr=new XMLHttpRequest();xhr.open('GET','/binary');xhr.responseType='arraybuffer';xhr.addEventListener('readystatechange',()=>states.push(xhr.readyState));xhr.onload=()=>{xhrResult=Array.from(new Uint8Array(xhr.response));};xhr.send();`)
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &message); err != nil {
		t.Fatal(err)
	}
	if err := h.receive(inputEvent{command: command{"id": message.Request["id"], "status": float64(200), "bodyBase64": "AID/", "headers": map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	drainTasks(t, h)
	if got := evalTest(t, r, `JSON.stringify({states,bytes:xhrResult})`).String(); got != `{"states":[2,3,4],"bytes":[0,128,255]}` {
		t.Fatal(got)
	}
}

func TestRemovedFrameSubtreeStopsTimers(t *testing.T) {
	h, r, _ := testRealm(t, "https://same.example/")
	evalTest(t, r, `var box=document.createElement('div'),frame=document.createElement('iframe');frame.src='/child';box.appendChild(frame);document.body.appendChild(box);var old=frame.contentWindow;old.setTimeout(()=>{throw Error('removed frame timer ran')},0);box.remove();if(frame.contentWindow!==null||!old.closed)throw Error('detached frame lifecycle');`)
	drainTasks(t, h)
	if len(r.frames) != 0 {
		t.Fatal("detached frames retained active context")
	}
}

func TestRunInterruptsScriptsAndMicrotaskChains(t *testing.T) {
	for _, source := range []string{`while(true){}`, `Promise.resolve().then(function spin(){return Promise.resolve().then(spin)})`} {
		t.Run(source[:min(12, len(source))], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
			defer cancel()
			input, _ := json.Marshal(map[string]any{"url": "https://example.test/", "loaderUrl": "https://example.test/loader.js", "loaderSource": source})
			start := time.Now()
			err := Run(ctx, "sentinel", input, strings.NewReader(""), io.Discard)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected deadline, got %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("interruption exceeded budget")
			}
		})
	}
}

func TestRunConcurrentRealmIsolation(t *testing.T) {
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			input, _ := json.Marshal(map[string]any{"url": "https://fixture.test/", "loaderUrl": "https://fixture.test/loader.js", "loaderSource": `globalThis.privateValue=__config.fixture;globalThis.SentinelSDK={token:()=>String(privateValue)}`, "fixture": i})
			var out bytes.Buffer
			err := Run(ctx, "sentinel", input, strings.NewReader("{\"type\":\"call\",\"method\":\"token\",\"flow\":\"fixture\"}\n{\"type\":\"shutdown\"}\n"), &out)
			if err != nil {
				t.Error(err)
				return
			}
			var result struct{ Value string }
			if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &result); err != nil {
				t.Error(err)
				return
			}
			var wanted bytes.Buffer
			_ = json.NewEncoder(&wanted).Encode(i)
			if result.Value != strings.TrimSpace(wanted.String()) {
				t.Error("cross-session value leaked")
			}
		}(i)
	}
	group.Wait()
}
