package jshost

import (
	"fmt"
	"testing"

	"github.com/grafana/sobek"
)

func webTestVM(t *testing.T, config map[string]any) *sobek.Runtime {
	t.Helper()
	vm := sobek.New()
	if config == nil {
		config = map[string]any{}
	}
	if err := vm.Set("__config", config); err != nil {
		t.Fatal(err)
	}
	if err := vm.Set("__host", map[string]any{"now": func() float64 { return 12.5 }}); err != nil {
		t.Fatal(err)
	}
	if err := installWebPlatform(vm); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`function assert(value,message="assertion failed"){if(!value)throw new Error(message);}function equal(actual,expected){assert(JSON.stringify(actual)===JSON.stringify(expected),JSON.stringify(actual)+" != "+JSON.stringify(expected));}`); err != nil {
		t.Fatal(err)
	}
	return vm
}

func runWebTest(t *testing.T, vm *sobek.Runtime, source string, asynchronous bool) {
	t.Helper()
	if asynchronous {
		source = `globalThis.__webTestDone=false;globalThis.__webTestFailure="";(async()=>{` + source + `})().then(()=>{__webTestDone=true;},error=>{__webTestFailure=String(error&&error.stack||error);__webTestDone=true;});`
	}
	if _, err := vm.RunScript("webplatform-test.js", source); err != nil {
		t.Fatal(err)
	}
	if asynchronous {
		if !vm.Get("__webTestDone").ToBoolean() {
			t.Fatal("asynchronous platform operation remained pending")
		}
		if failure := vm.Get("__webTestFailure").String(); failure != "" {
			t.Fatal(failure)
		}
	}
}

func TestWebURLWHATWGAndLinkedSearchParams(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), `
const u=new URL("../a b?x=hello world#片","https://例え.テスト:443/x/y");
equal([u.href,u.origin,u.hostname,u.port],["https://xn--r8jz45g.xn--zckzah/a%20b?x=hello%20world#%E7%89%87","https://xn--r8jz45g.xn--zckzah","xn--r8jz45g.xn--zckzah",""]);
const v=new URL("https://EXAMPLE.com:443/a?b=2&a=1&a=3&mark=~");const p=v.searchParams;p.delete("a","1");p.append("a","4");p.sort();equal(v.href,"https://example.com/a?a=3&a=4&b=2&mark=%7E");
v.search="?x=1&x=2";assert(v.searchParams===p);equal(p.getAll("x"),["1","2"]);p.set("x","A B");equal(v.search,"?x=A+B");
v.pathname="/a b/é";equal(v.pathname,"/a%20b/%C3%A9");v.protocol="http:";v.port="80";equal(v.origin,"http://example.com");
equal(new URL("http://0x7f000001/").hostname,"127.0.0.1");equal(new URL("https://[::1]:443/").host,"[::1]");
assert(!URL.canParse("http://[bad"));assert(URL.parse("/relative")===null);assert(!URL.canParse("https://example.com/",""));
equal(new URLSearchParams("a=%E2%82&b=%ZZ&c=1+2").get("a"),"�");equal(new URLSearchParams("b=%ZZ").get("b"),"%ZZ");
equal(new URLSearchParams([["*~"," +"]]).toString(),"*%7E=+%2B");
equal(new URLSearchParams({x:"\ud800"}).get("x"),"�");
`, false)
}

func TestWebTextUnicodeStreamingAndBase64(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), `
const encoder=new TextEncoder();equal(Array.from(encoder.encode("€😀\ud800")),[226,130,172,240,159,152,128,239,191,189]);
const dest=new Uint8Array(5);equal(encoder.encodeInto("A😀B",dest),{read:3,written:5});equal(Array.from(dest),[65,240,159,152,128]);
const small=new Uint8Array(4);equal(encoder.encodeInto("A😀",small),{read:1,written:1});
const decoder=new TextDecoder();equal(decoder.decode(new Uint8Array([226]),{stream:true}),"");equal(decoder.decode(new Uint8Array([130,172])),"€");
equal(new TextDecoder().decode(new Uint8Array([226,130])),"�");
const bom=new TextDecoder();equal(bom.decode(new Uint8Array([239]),{stream:true}),"");equal(bom.decode(new Uint8Array([187,191,65])),"A");
equal(new TextDecoder("utf-8",{ignoreBOM:true}).decode(new Uint8Array([239,187,191,65])),"\ufeffA");
equal(new TextDecoder("utf-16").decode(new Uint8Array([255,254,65,0,61,216,0,222])),"A😀");
equal(new TextDecoder("latin1").decode(new Uint8Array([128])),"€");
let rejected=false;try{new TextDecoder("utf-8",{fatal:true}).decode(new Uint8Array([255]));}catch(e){rejected=e instanceof TypeError;}assert(rejected,"fatal UTF8 accepted invalid bytes");
equal(btoa("\x00\xff\x80"),"AP+A");equal(Array.from(atob("AP+A"),c=>c.charCodeAt(0)),[0,255,128]);equal(atob(" YQ==\n"),"a");equal(atob("YQ"),"a");
for(const value of ["YQ=","%"]){let rejected=false;try{atob(value);}catch(e){rejected=e.name==="InvalidCharacterError";}assert(rejected,"invalid base64 accepted");}
let badLatin1=false;try{btoa("中文");}catch(e){badLatin1=e.name==="InvalidCharacterError";}assert(badLatin1);
assert(!Object.keys(globalThis).includes("__webBlobBytes"));assert(typeof __webPlatformBridge==="undefined");
`, false)
}

func TestWebBlobAndFetchBodiesPreserveBinaryOwnership(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), `
const original=new Uint8Array([9,0,255,128,10]);const view=original.subarray(1,4);
const blob=new Blob([view,"€",new Blob(["A"])],{type:"APPLICATION/OCTET-STREAM"});original.fill(7);
equal([blob.size,blob.type],[7,"application/octet-stream"]);equal(Array.from(new Uint8Array(await blob.arrayBuffer())),[0,255,128,226,130,172,65]);equal(Array.from(new Uint8Array(await blob.slice(-4,-1).arrayBuffer())),[226,130,172]);
equal(await new Blob([null,undefined]).text(),"nullundefined");
const saved=__webBlobBytes(blob);saved.fill(0);assert(__webBlobBytes(blob)[1]===255,"Blob bridge exposed owned bytes");
const r=new Response(__webBlobBytes(blob),{headers:{"Content-Type":"application/octet-stream"}});const clone=r.clone();
equal(Array.from(new Uint8Array(await r.arrayBuffer())),[0,255,128,226,130,172,65]);assert(r.bodyUsed);assert(!clone.bodyUsed);equal(Array.from(new Uint8Array(await clone.arrayBuffer())),[0,255,128,226,130,172,65]);
let used=false;try{await r.text();}catch(e){used=e instanceof TypeError;}assert(used);
const request=new Request("https://example.com/a",{method:"post",body:new Uint8Array([0,255]),headers:{"X-A":"one"}});const copy=request.clone();request.headers.set("x-a","changed");
equal([copy.method,copy.headers.get("x-a")],["POST","one"]);equal(Array.from(new Uint8Array(await request.arrayBuffer())),[0,255]);equal(Array.from(new Uint8Array(await copy.arrayBuffer())),[0,255]);
const empty=new Request("https://example.com/");equal((await empty.arrayBuffer()).byteLength,0);equal((await empty.arrayBuffer()).byteLength,0);assert(!empty.bodyUsed&&empty.body===null);
equal(await Response.json({x:1}).json(),{x:1});equal((await new Response("body",{headers:{"content-type":"TEXT/PLAIN"}}).blob()).type,"text/plain");
let getBody=false;try{new Request("https://example.com/",{body:"value"});}catch(e){getBody=true;}assert(getBody);
let invalid204=false;try{new Response(new Uint8Array(0),{status:204});}catch(e){invalid204=true;}assert(invalid204);assert(new Response(null,{status:204}).body===null);
`, true)
}

func TestWebBodyReaderLockAndReleaseNeverReplay(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), `
const response=new Response(new Uint8Array([1,2,3]));const reader=response.body.getReader();assert(response.body.locked&&!response.bodyUsed);
let locked=false;try{response.clone();}catch(e){locked=true;}assert(locked);let readLocked=false;try{await response.text();}catch(e){readLocked=true;}assert(readLocked);
equal(Array.from((await reader.read()).value),[1,2,3]);assert(response.bodyUsed);reader.releaseLock();assert(!response.body.locked);
const next=response.body.getReader();assert((await next.read()).done,"new reader replayed drained bytes");next.releaseLock();
const unread=new Response(new Uint8Array([4]));const first=unread.body.getReader();first.releaseLock();const second=unread.body.getReader();equal(Array.from((await second.read()).value),[4]);second.releaseLock();
const consumed=new Response("text");await consumed.text();const drained=consumed.body.getReader();assert((await drained.read()).done);drained.releaseLock();
const canceled=new Response("text");await canceled.body.cancel();assert(canceled.bodyUsed);assert((await canceled.body.getReader().read()).done);
const stream=new Blob([new Uint8Array([5,6])]).stream();const branches=stream.tee();equal(Array.from((await branches[0].getReader().read()).value),[5,6]);equal(Array.from((await branches[1].getReader().read()).value),[5,6]);
`, true)
}

func TestWebReadableStreamAsyncStartPullAndBackpressure(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), `
const tick=async()=>{for(let i=0;i<12;i++)await Promise.resolve();};
let startResolve,pullResolve,pulls=0,active=0,maxActive=0;
const startGate=new Promise(resolve=>startResolve=resolve),pullGate=new Promise(resolve=>pullResolve=resolve);
const stream=new ReadableStream({
  async start(controller){await startGate;},
  async pull(controller){active++;maxActive=Math.max(maxActive,active);await pullGate;controller.enqueue(++pulls);if(pulls===3)controller.close();active--;}
},{highWaterMark:0});
assert(stream instanceof ReadableStream);await tick();equal(pulls,0);
const reader=stream.getReader();const reads=[reader.read(),reader.read(),reader.read()];
await tick();equal(active,0);startResolve();await tick();equal(active,1);equal(pulls,0);
pullResolve();equal((await Promise.all(reads)).map(result=>result.value),[1,2,3]);equal(maxActive,1);
assert((await reader.read()).done);await reader.closed;reader.releaseLock();

let controller,count=0;
const measured=new ReadableStream({start(c){controller=c;},pull(c){count++;c.enqueue(new Uint8Array(2));if(count===3)c.close();}},new ByteLengthQueuingStrategy({highWaterMark:4}));
await tick();equal(count,2);equal(controller.desiredSize,0);
const measuredReader=measured.getReader();equal((await measuredReader.read()).value.byteLength,2);await tick();equal(count,3);
equal((await measuredReader.read()).value.byteLength,2);equal((await measuredReader.read()).value.byteLength,2);assert((await measuredReader.read()).done);equal(controller.desiredSize,0);

let closeController;
const closing=new ReadableStream({start(c){closeController=c;c.enqueue("queued");c.close();}});const closingReader=closing.getReader();
let closed=false;closingReader.closed.then(()=>closed=true);await tick();assert(!closed,"close discarded queued data");
equal((await closingReader.read()).value,"queued");await closingReader.closed;assert(closed);assert((await closingReader.read()).done);
let badEnqueue=false;try{closeController.enqueue("late");}catch(e){badEnqueue=e instanceof TypeError;}assert(badEnqueue);
equal(new CountQueuingStrategy({highWaterMark:5}).size("chunk"),1);
let sizeThis="unset",sizeController;
new ReadableStream({start(c){sizeController=c;}},{highWaterMark:0,size:function(chunk){"use strict";sizeThis=this;return 1;}});sizeController.enqueue("chunk");assert(sizeThis===undefined);
`, true)
}

func TestWebReadableStreamErrorCancelAndReleasedPendingRead(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), `
const expectReject=async(promise,reason)=>{let rejected=false;try{await promise;}catch(e){rejected=reason===undefined?e instanceof TypeError:e===reason;}assert(rejected,"wrong rejection reason");};
let controller;
const stream=new ReadableStream({start(c){controller=c;}},{highWaterMark:0});const reader=stream.getReader();const pending=reader.read();
const oldClosed=reader.closed;reader.releaseLock();await expectReject(pending);await expectReject(oldClosed);assert(!stream.locked);
controller.enqueue("after-release");const next=stream.getReader();equal((await next.read()).value,"after-release");
const failure=new Error("source-error"),waiting=next.read();controller.error(failure);await expectReject(waiting,failure);await expectReject(next.closed,failure);await expectReject(next.cancel(),failure);next.releaseLock();

const failedStart=new Error("start-error");const startStream=new ReadableStream({start(){return Promise.reject(failedStart);}});const startReader=startStream.getReader();await expectReject(startReader.read(),failedStart);await expectReject(startReader.closed,failedStart);
const failedPull=new Error("pull-error");const pullStream=new ReadableStream({pull(){throw failedPull;}},{highWaterMark:0});await expectReject(pullStream.getReader().read(),failedPull);
let sizeController;const invalidSize=new ReadableStream({start(c){sizeController=c;}},{size(){return -1;}});let sizeError;
try{sizeController.enqueue("chunk");}catch(e){sizeError=e;}assert(sizeError instanceof RangeError);await expectReject(invalidSize.getReader().read(),sizeError);

let cancelResolve,cancelCalls=0,cancelReason;
const canceled=new ReadableStream({cancel(reason){cancelCalls++;cancelReason=reason;return new Promise(resolve=>cancelResolve=resolve);}},{highWaterMark:0});
const canceledReader=canceled.getReader(),pendingCanceled=canceledReader.read();await expectReject(canceled.cancel("locked"));
const cancelPromise=canceledReader.cancel("stop");assert((await pendingCanceled).done);await canceledReader.closed;equal(cancelReason,"stop");equal(cancelCalls,1);cancelResolve();await cancelPromise;await canceledReader.cancel("again");equal(cancelCalls,1);
const cancelError=new Error("cancel failed"),badCancel=new ReadableStream({cancel(){throw cancelError;}});await expectReject(badCancel.cancel(),cancelError);assert((await badCancel.getReader().read()).done);

let byteError=false;try{new ReadableStream({type:"bytes"});}catch(e){byteError=e.name==="NotSupportedError";}assert(byteError);
let byobError=false;try{new ReadableStream().getReader({mode:"byob"});}catch(e){byobError=e.name==="NotSupportedError";}assert(byobError);
for(const callback of [()=>new ReadableStreamDefaultController(),()=>new ReadableStreamDefaultReader({}),()=>new ReadableStream({pull:1}),()=>new ReadableStream({},{highWaterMark:-1})]){let failed=false;try{callback();}catch{failed=true;}assert(failed);}
`, true)
}

func TestWebReadableStreamTeeIteratorAndStreamBodies(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), `
const all=async stream=>{const reader=stream.getReader(),values=[];try{while(true){const result=await reader.read();if(result.done)return values;values.push(result.value);}}finally{reader.releaseLock();}};
let count=0;const original=new ReadableStream({pull(c){c.enqueue(++count);if(count===4)c.close();}},{highWaterMark:0});
const [first,second]=original.tee();assert(original.locked);equal(await all(first),[1,2,3,4]);equal(await all(second),[1,2,3,4]);assert(original.locked);

let reasons,cancelCalls=0;const cancellable=new ReadableStream({cancel(value){reasons=value;cancelCalls++;}},{highWaterMark:0});const branches=cancellable.tee();
const one=branches[0].cancel("one"),two=branches[1].cancel("two");await Promise.all([one,two]);equal(reasons,["one","two"]);equal(cancelCalls,1);assert(cancellable.locked);
const finite=new ReadableStream({start(c){c.enqueue(1);c.enqueue(2);c.close();}});const split=finite.tee();let oneDone=false;const oneCancel=split[0].cancel("skip").then(()=>oneDone=true);
await Promise.resolve();assert(!oneDone);equal(await all(split[1]),[1,2]);await oneCancel;assert(oneDone);
let errorController;const errored=new ReadableStream({start(c){errorController=c;}}),errors=errored.tee(),failure=new Error("tee-error");errorController.error(failure);
for(const branch of errors){let bad=false;try{await branch.getReader().read();}catch(e){bad=e===failure;}assert(bad);}

let stopped;const iterable=new ReadableStream({start(c){c.enqueue(1);c.enqueue(2);},cancel(reason){stopped=reason;}});const iterator=iterable.values();equal((await iterator.next()).value,1);equal(await iterator.return("end"),{value:"end",done:true});equal(stopped,"end");assert(!iterable.locked);
const retained=new ReadableStream({start(c){c.enqueue(1);c.enqueue(2);c.close();}});const retainedIterator=retained.values({preventCancel:true});await retainedIterator.next();await retainedIterator.return();equal(await all(retained),[2]);

const response=new Response(new ReadableStream({start(c){c.enqueue(new Uint8Array([65]));c.enqueue(new Uint8Array([226,130,172]));c.close();}}));const copy=response.clone();assert(!response.bodyUsed&&!copy.bodyUsed);equal(await response.text(),"A€");equal(await copy.text(),"A€");assert(response.bodyUsed&&copy.bodyUsed);
let canceledBadBody=false;const badBody=new Response(new ReadableStream({start(c){c.enqueue("not-bytes");},cancel(){canceledBadBody=true;}}));let bad=false;try{await badBody.text();}catch(e){bad=e instanceof TypeError;}assert(bad&&canceledBadBody&&badBody.bodyUsed);
`, true)
}

func TestWebRequestTransfersBodyButCloneKeepsBothReadable(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), `
const original=new Request("https://example.com/",{method:"POST",body:"original",headers:{"x-one":"a"},credentials:"include"});const priorBody=original.body;
const transferred=new Request(original);assert(original.bodyUsed&&original.body.locked&&!transferred.bodyUsed);assert(original.body===priorBody&&transferred.body!==priorBody);equal(await transferred.text(),"original");assert(original.body.locked);
let blocked=false;try{await original.text();}catch(e){blocked=e instanceof TypeError;}assert(blocked);
let transferAgain=false;try{new Request(original);}catch(e){transferAgain=e instanceof TypeError;}assert(transferAgain);
const override=new Request(original,{body:"replacement"});equal(await override.text(),"replacement");
const source=new Request("https://example.com/",{method:"POST",body:"clone",credentials:"include"});source.body;const clone=source.clone();assert(!source.bodyUsed&&!source.body.locked&&!clone.bodyUsed);equal([clone.credentials,await source.text(),await clone.text()],["include","clone","clone"]);
for(const value of [null,undefined]){const prior=new Request("https://example.com/",{method:"POST",body:"retained"});const next=new Request(prior,{body:value});assert(prior.bodyUsed);equal(await next.text(),"retained");}
const empty=new Request("https://example.com/");new Request(empty);assert(!empty.bodyUsed&&empty.body===null);
const invalid=new Request("https://example.com/",{method:"POST",body:"untouched"});let invalidHeaders=false;try{new Request(invalid,{headers:{"bad name":"v"}});}catch(e){invalidHeaders=e instanceof TypeError;}assert(invalidHeaders&&!invalid.bodyUsed&&!invalid.body.locked);equal(await invalid.text(),"untouched");
`, true)
}

const pipeFixture = `
function sink(options={}){
  let locked=false,resolveClosed,rejectClosed;const closed=new Promise((resolve,reject)=>{resolveClosed=resolve;rejectClosed=reject;});closed.catch(()=>{});
  const writes=[],aborts=[];let closes=0,releases=0;
  return {writes,aborts,get closes(){return closes;},get releases(){return releases;},get locked(){return locked;},fail:rejectClosed,
    getWriter(){if(locked)throw new TypeError("locked");locked=true;return {ready:options.ready||Promise.resolve(),closed,
      async write(value){if(options.write)await options.write(value);writes.push(value);},
      async close(){closes++;if(options.close)await options.close();resolveClosed();},
      async abort(reason){aborts.push(reason);if(options.abort)await options.abort(reason);rejectClosed(reason);},
      releaseLock(){locked=false;releases++;}
    };}
  };
}
async function rejected(promise,reason){let bad=false;try{await promise;}catch(e){bad=e===reason;}assert(bad,"unexpected pipe rejection");}
`

func TestWebReadableStreamPipeToOrdersWritesAndPropagatesFailures(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), pipeFixture+`
let count=0,active=0,maxActive=0;const ordered=sink({async write(value){active++;maxActive=Math.max(maxActive,active);await Promise.resolve();active--;}});
const source=new ReadableStream({pull(c){c.enqueue(++count);if(count===3)c.close();}},{highWaterMark:0});await source.pipeTo(ordered);equal(ordered.writes,[1,2,3]);equal([ordered.closes,ordered.releases,maxActive],[1,1,1]);assert(!source.locked&&!ordered.locked);
const preserved=sink();await new ReadableStream({start(c){c.close();}}).pipeTo(preserved,{preventClose:true});equal(preserved.closes,0);assert(!preserved.locked);

const writeError=new Error("write failed");let cancellation;
const badDestination=sink({write(){return Promise.reject(writeError);}});const cancelSource=new ReadableStream({pull(c){c.enqueue(1);},cancel(reason){cancellation=reason;}},{highWaterMark:0});await rejected(cancelSource.pipeTo(badDestination),writeError);assert(cancellation===writeError);assert(!cancelSource.locked&&!badDestination.locked);
let preventCanceled=false;const keepSource=new ReadableStream({start(c){c.enqueue(7);c.enqueue(8);c.close();},cancel(){preventCanceled=true;}});await rejected(keepSource.pipeTo(sink({write(){throw writeError;}}),{preventCancel:true}),writeError);assert(!preventCanceled&&!keepSource.locked);equal((await keepSource.getReader().read()).value,8);

const sourceError=new Error("source failed");let errorController;const errorSource=new ReadableStream({start(c){errorController=c;c.enqueue(1);}});
const stalled=sink({write(){return new Promise(()=>{});}});const piping=errorSource.pipeTo(stalled);for(let i=0;i<12;i++)await Promise.resolve();errorController.error(sourceError);await rejected(piping,sourceError);assert(stalled.aborts[0]===sourceError);assert(!errorSource.locked&&!stalled.locked);
const suppressAbort=sink();await rejected(new ReadableStream({start(c){c.error(sourceError);}}).pipeTo(suppressAbort,{preventAbort:true}),sourceError);equal(suppressAbort.aborts,[]);

const externalError=new Error("destination externally failed");let pendingCancel;const idleSource=new ReadableStream({cancel(reason){pendingCancel=reason;}},{highWaterMark:0});const external=sink(),externalPipe=idleSource.pipeTo(external);for(let i=0;i<6;i++)await Promise.resolve();external.fail(externalError);await rejected(externalPipe,externalError);assert(pendingCancel===externalError);assert(!idleSource.locked&&!external.locked);
`, true)
}

func TestWebReadableStreamPipeAbortAndTransform(t *testing.T) {
	runWebTest(t, webTestVM(t, nil), pipeFixture+`
function signal(){const callbacks=new Set();return {aborted:false,reason:undefined,addEventListener(type,fn){callbacks.add(fn);},removeEventListener(type,fn){callbacks.delete(fn);},abort(reason){this.aborted=true;this.reason=reason;for(const fn of callbacks)fn();},get listenerCount(){return callbacks.size;}};}
const reason=new Error("aborted"),abortSignal=signal();let cancelReason;const source=new ReadableStream({cancel(value){cancelReason=value;}},{highWaterMark:0});const destination=sink({ready:new Promise(()=>{})}),piping=source.pipeTo(destination,{signal:abortSignal});abortSignal.abort(reason);await rejected(piping,reason);assert(cancelReason===reason&&destination.aborts[0]===reason);equal(abortSignal.listenerCount,0);assert(!source.locked&&!destination.locked);
const already=signal();already.abort(reason);let canceled=false;const preservedSource=new ReadableStream({cancel(){canceled=true;}},{highWaterMark:0}),preservedSink=sink();await rejected(preservedSource.pipeTo(preservedSink,{signal:already,preventAbort:true,preventCancel:true}),reason);assert(!canceled);equal(preservedSink.aborts,[]);assert(!preservedSource.locked&&!preservedSink.locked);

let transformController;const readable=new ReadableStream({start(c){transformController=c;}});const writable=sink({write(value){transformController.enqueue(value*2);},close(){transformController.close();}});
const transformed=new ReadableStream({start(c){c.enqueue(1);c.enqueue(2);c.close();}}).pipeThrough({readable,writable});assert(transformed===readable);const reader=transformed.getReader();equal((await reader.read()).value,2);equal((await reader.read()).value,4);assert((await reader.read()).done);
let invalid=false;try{new ReadableStream().pipeThrough({readable:{},writable});}catch(e){invalid=e instanceof TypeError;}assert(invalid);
const unmodified=new ReadableStream({start(c){c.enqueue(9);}}),validSink=sink();let invalidSignal=false;try{await unmodified.pipeTo(validSink,{signal:{}});}catch(e){invalidSignal=e instanceof TypeError;}assert(invalidSignal&&!unmodified.locked&&!validSink.locked);equal((await unmodified.getReader().read()).value,9);
`, true)
}

func TestWebHeadersAndCryptoVectors(t *testing.T) {
	vm := webTestVM(t, nil)
	runWebTest(t, vm, `
const h=new Headers([["X-B"," one "],["x-b","two"],["Set-Cookie","a=1"],["set-cookie","b=2"],["A","first"]]);equal(h.get("X-B"),"one, two");equal(h.getSetCookie(),["a=1","b=2"]);equal(Array.from(h.entries()),[["a","first"],["set-cookie","a=1"],["set-cookie","b=2"],["x-b","one, two"]]);
for(const pair of [["bad name","v"],["name","bad\r\nvalue"],["name","中文"]]){let bad=false;try{h.append(...pair);}catch(e){bad=true;}assert(bad,"invalid header accepted");}
const target=new Uint8Array(32);const result=crypto.getRandomValues(target.subarray(8,24));assert(result.buffer===target.buffer);assert(target.slice(0,8).every(x=>x===0)&&target.slice(24).every(x=>x===0));assert(target.slice(8,24).some(x=>x!==0));
assert(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(crypto.randomUUID()));
let wrongType=false;try{crypto.getRandomValues(new Float32Array(2));}catch(e){wrongType=e.name==="TypeMismatchError";}assert(wrongType);
let quota=false;try{crypto.getRandomValues(new Uint8Array(65537));}catch(e){quota=e.name==="QuotaExceededError";}assert(quota);
const hex=buffer=>Array.from(new Uint8Array(buffer),b=>b.toString(16).padStart(2,"0")).join("");const source=new Uint8Array([0,97,98,99,255]);
equal(hex(await crypto.subtle.digest("SHA-256",source.subarray(1,4))),"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
equal(hex(await crypto.subtle.digest({name:"sha-1"},new TextEncoder().encode("abc"))),"a9993e364706816aba3e25717850c26c9cd0d89d");
equal(hex(await crypto.subtle.digest("SHA-384",new TextEncoder().encode("abc"))),"cb00753f45a35e8bb5a03d699ac65007272c32ab0eded1631a8b605a43ff5bed8086072ba1e7cc2358baeca134c825a7");
equal(hex(await crypto.subtle.digest("SHA-512",new TextEncoder().encode("abc"))),"ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f");
let unsupported=false;try{await crypto.subtle.digest("MD5",source);}catch(e){unsupported=e.name==="NotSupportedError";}assert(unsupported);
`, true)
}

func TestWebPerformanceInputFilteringAndObserver(t *testing.T) {
	vm := webTestVM(t, map[string]any{"performanceEntries": []any{map[string]any{"name": "https://example.com/", "entryType": "navigation", "startTime": 0, "duration": 400}, map[string]any{"name": "sdk.js", "entryType": "resource", "startTime": 10, "duration": 15, "initiatorType": "script"}}, "performanceMemory": map[string]any{"jsHeapSizeLimit": 12345, "usedJSHeapSize": 77}, "timeOrigin": 1000})
	runWebTest(t, vm, `
equal(performance.now(),12.5);equal(performance.timeOrigin,1000);equal(performance.memory.jsHeapSizeLimit,12345);equal(performance.memory.usedJSHeapSize,77);
const nav=performance.getEntriesByType("navigation")[0];assert(nav instanceof PerformanceNavigationTiming&&nav instanceof PerformanceResourceTiming&&nav instanceof PerformanceEntry);equal(nav.duration,400);equal(performance.getEntriesByName("sdk.js","resource").length,1);equal(performance.getEntriesByName("sdk.js","navigation").length,0);
let observed=[];const observer=new PerformanceObserver(list=>observed.push(...list.getEntries().map(entry=>entry.name)));observer.observe({type:"resource",buffered:true});await Promise.resolve();equal(observed,["sdk.js"]);
performance.__addEntry({name:"actual.css",entryType:"resource",startTime:12.5,duration:2});await Promise.resolve();equal(observed,["sdk.js","actual.css"]);
observer.disconnect();performance.__addEntry({name:"ignored.css",entryType:"resource"});await Promise.resolve();equal(observed,["sdk.js","actual.css"]);
performance.mark("a",{startTime:1});performance.mark("b",{startTime:4});equal(performance.measure("duration","a","b").duration,3);performance.clearResourceTimings();equal(performance.getEntriesByType("resource").length,0);equal(performance.getEntriesByType("navigation").length,1);
`, true)
}

func TestWebPlatformNoSharedStateAcrossRuntimes(t *testing.T) {
	a, b := webTestVM(t, nil), webTestVM(t, nil)
	runWebTest(t, a, `URL.prototype.realmOnly=1;crypto.realmOnly=1;performance.mark("private");`, false)
	runWebTest(t, b, `assert(URL.prototype.realmOnly===undefined&&crypto.realmOnly===undefined);assert(performance.getEntries().length===0);`, false)
	if fmt.Sprint(a.Get("URL")) == "" {
		t.Fatal("missing URL constructor")
	}
}
